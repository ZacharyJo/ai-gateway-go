package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggerFileOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-gateway.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	l := NewLogger(nil, true) // 无 stderr 输出，仅文件
	l.SetFile(f)
	l.Infof("hello req=%d", 1)
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "hello req=1") {
		t.Errorf("file log = %q, want hello req=1", string(b))
	}
	if !strings.Contains(string(b), "[proxy] info") {
		t.Errorf("file log missing level prefix: %q", string(b))
	}
}

func TestRotatingWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-gateway.log")
	w, err := NewRotatingWriter(path, 100, 3, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	// 写超过 100 字节，触发轮转
	for i := 0; i < 30; i++ {
		if _, err := w.Write([]byte("0123456789abcdef\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// 等轮转 goroutine 跑几轮
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path + ".1"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation did not happen: %v", err)
	}
	// 主文件重新开始，大小应 <= 上限（刚轮转后为 0 或少量）
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat main log: %v", err)
	}
	if fi.Size() > 200 {
		t.Errorf("main log size = %d after rotation, want <= 200", fi.Size())
	}
}

func TestRotatingWriterKeep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-gateway.log")
	w, err := NewRotatingWriter(path, 50, 2, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	// 大量写入触发多次轮转
	for i := 0; i < 200; i++ {
		w.Write([]byte("0123456789abcdef\n"))
	}
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path + ".2"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// keep=2：允许 log.1、log.2，不允许 log.3
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("log.3 exists, want rotated away (keep=2)")
	}
}
