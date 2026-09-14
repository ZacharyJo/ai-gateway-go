package proxy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger 输出单行紧凑日志。
// 默认只打印 warn/error 与 request_finish；DIAGNOSTIC_LOGGING=1 时打印全部（Infof）。
// 同时写到 stderr（前台可见）与可选的轮转文件（LOG_DIR 配置时）。
type Logger struct {
	mu      sync.Mutex
	out     io.Writer
	file    io.Writer // 可选：轮转文件
	verbose bool
}

// NewLogger 构造日志器。out 为 nil 时丢弃输出。
func NewLogger(out io.Writer, verbose bool) *Logger {
	if out == nil {
		out = io.Discard
	}
	return &Logger{out: out, verbose: verbose}
}

// SetFile 附加轮转文件输出（LOG_DIR 配置时由 Server 设置）。
func (l *Logger) SetFile(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.file = w
}

// Infof 打印普通日志（默认抑制，DIAGNOSTIC_LOGGING=1 才显示）。
func (l *Logger) Infof(format string, args ...any) {
	if !l.verbose {
		return
	}
	l.printf("info", format, args...)
}

// Requestf 打印请求生命周期日志（request_received/request_finish），始终显示。
func (l *Logger) Requestf(format string, args ...any) {
	l.printf("info", format, args...)
}

// Warnf 打印警告日志（重试等），始终显示。
func (l *Logger) Warnf(format string, args ...any) {
	l.printf("warn", format, args...)
}

// Errorf 打印错误日志，始终显示。
func (l *Logger) Errorf(format string, args ...any) {
	l.printf("error", format, args...)
}

func (l *Logger) printf(level, format string, args ...any) {
	line := fmt.Sprintf("%s [proxy] %s %s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), level, fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		_, _ = io.WriteString(l.out, line)
	}
	if l.file != nil {
		_, _ = io.WriteString(l.file, line)
	}
}

// RotatingWriter 是带轮转的日志文件写入器。
// 自己持有 fd，用 rename 轮转（log → log.1 → log.2 ...），周期检查大小。
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	interval time.Duration
	file     *os.File
	size     int64
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewRotatingWriter 构造轮转写入器并启动周期检查 goroutine。调用方负责 Close。
func NewRotatingWriter(path string, maxBytes int64, keep int, interval time.Duration) (*RotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	w := &RotatingWriter{
		path: path, maxBytes: maxBytes, keep: max(1, keep), interval: interval,
		file: f, size: fi.Size(), stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
	go w.loop()
	return w, nil
}

// Write 实现 io.Writer。
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 停止周期检查并关闭文件。
func (w *RotatingWriter) Close() error {
	close(w.stopCh)
	<-w.doneCh
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// loop 周期检查文件大小，超限轮转。
func (w *RotatingWriter) loop() {
	defer close(w.doneCh)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.mu.Lock()
			over := w.size > w.maxBytes
			w.mu.Unlock()
			if over {
				w.rotate()
			}
		}
	}
}

// rotate 用 rename 轮转：log → log.1 → log.2 ...（保留 keep 份）。
func (w *RotatingWriter) rotate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return
	}
	if err := w.file.Close(); err != nil {
		return
	}
	// 从旧到新移位：log.(keep-1) → log.keep, ..., log.1 → log.2
	for i := w.keep - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", w.path, i)
		next := fmt.Sprintf("%s.%d", w.path, i+1)
		_ = os.Rename(old, next) // ENOENT 忽略
	}
	_ = os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.file = nil
		return
	}
	w.file = f
	w.size = 0
}
