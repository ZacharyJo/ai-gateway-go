package proxy

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowReader 每次 Read 前先等 delay，用来模拟上游长时间不吐字节。
type slowReader struct {
	delay  time.Duration
	data   string
	done   bool
	closed atomic.Bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.done {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	s.done = true
	n := copy(p, s.data)
	return n, nil
}

func (s *slowReader) Close() error {
	s.closed.Store(true)
	return nil
}

func TestIdleTimeoutReaderFiresWhenIdle(t *testing.T) {
	var tripped atomic.Bool
	src := &slowReader{delay: 200 * time.Millisecond, data: "late"}
	rc := newIdleTimeoutReader(src, 30*time.Millisecond, func() { tripped.Store(true) })
	defer rc.Close()

	buf := make([]byte, 16)
	_, _ = rc.Read(buf) // 读期间超过空闲阈值 → onIdle 触发
	if !tripped.Load() {
		t.Error("idle timeout did not fire while upstream stayed silent")
	}
}

func TestIdleTimeoutReaderResetsOnData(t *testing.T) {
	var tripped atomic.Bool
	// 每次 Read 立刻返回数据；累计时长远超空闲阈值，但因为一直有字节所以不该触发
	src := io.NopCloser(strings.NewReader(strings.Repeat("data: x\n\n", 50)))
	rc := newIdleTimeoutReader(src, 60*time.Millisecond, func() { tripped.Store(true) })
	defer rc.Close()

	buf := make([]byte, 8) // 小 buffer → 多次 Read
	for {
		_, err := rc.Read(buf)
		if err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond) // 间隔远小于阈值
	}
	if tripped.Load() {
		t.Error("idle timeout fired although upstream kept sending bytes")
	}
}

func TestIdleTimeoutReaderDisabled(t *testing.T) {
	src := io.NopCloser(strings.NewReader("x"))
	// timeout <= 0 或 onIdle 为 nil → 不包装，原样返回
	if rc := newIdleTimeoutReader(src, 0, func() {}); rc != src {
		t.Error("timeout=0 should return the original body unwrapped")
	}
	if rc := newIdleTimeoutReader(src, time.Second, nil); rc != src {
		t.Error("nil onIdle should return the original body unwrapped")
	}
}

func TestIdleTimeoutReaderCloseStopsTimer(t *testing.T) {
	var tripped atomic.Bool
	src := io.NopCloser(strings.NewReader("x"))
	rc := newIdleTimeoutReader(src, 30*time.Millisecond, func() { tripped.Store(true) })
	rc.Close() // 关闭后计时器必须停掉，不能事后误触发
	time.Sleep(80 * time.Millisecond)
	if tripped.Load() {
		t.Error("idle timer fired after Close")
	}
}

func TestRetentionSweepRemovesExpired(t *testing.T) {
	logDir := t.TempDir()
	storeDir := filepath.Join(logDir, "headroom-lite-store")
	capDir := filepath.Join(logDir, "ai-gateway-captures")
	for _, d := range []string{storeDir, capDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(storeDir, "old.txt")
	fresh := filepath.Join(storeDir, "fresh.txt")
	oldCap := filepath.Join(capDir, "old.json")
	for _, f := range []string{old, fresh, oldCap} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 把两个文件的修改时间推到 48 小时前
	past := time.Now().Add(-48 * time.Hour)
	for _, f := range []string{old, oldCap} {
		if err := os.Chtimes(f, past, past); err != nil {
			t.Fatal(err)
		}
	}

	r := &Retention{logDir: logDir, maxAge: 24 * time.Hour, log: NewLogger(nil, false)}
	r.sweep()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("expired headroom original was not removed")
	}
	if _, err := os.Stat(oldCap); !os.IsNotExist(err) {
		t.Error("expired capture was not removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh file was removed: %v", err)
	}
}

func TestNewRetentionDisabled(t *testing.T) {
	if r := NewRetention(t.TempDir(), 0, nil); r != nil {
		t.Error("maxAge=0 should disable retention")
		r.Close()
	}
	if r := NewRetention("", time.Hour, nil); r != nil {
		t.Error("empty logDir should disable retention")
		r.Close()
	}
	// Close 对 nil 接收者安全
	var nilRetention *Retention
	nilRetention.Close()
}

func TestRetentionSweepMissingDirs(t *testing.T) {
	// 目录还没建出来时不能 panic
	r := &Retention{logDir: filepath.Join(t.TempDir(), "nope"), maxAge: time.Hour}
	r.sweep()
}
