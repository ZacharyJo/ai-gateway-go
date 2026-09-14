package proxy

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// idleReaderFor 构造一个可直接操纵内部状态的 idleTimeoutReader（白盒，用于测边界竞态）。
func idleReaderFor(t *testing.T, timeout time.Duration, onIdle func()) *idleTimeoutReader {
	t.Helper()
	rc := newIdleTimeoutReader(io.NopCloser(strings.NewReader("x")), timeout, onIdle)
	r, ok := rc.(*idleTimeoutReader)
	if !ok {
		t.Fatalf("newIdleTimeoutReader 返回了 %T，期望 *idleTimeoutReader", rc)
	}
	return r
}

func TestIdleTripIgnoredWhenDataJustArrived(t *testing.T) {
	// A3 回归：AfterFunc 派发后 Read 的 timer.Reset 拦不住它。
	// 字节恰在超时边界到达时，这次 trip 属于误判，不能取消连接。
	var tripped atomic.Bool
	r := idleReaderFor(t, 50*time.Millisecond, func() { tripped.Store(true) })
	defer r.Close()

	// 模拟"刚刚读到字节"，随后已派发的 trip 才拿到锁
	r.mu.Lock()
	r.lastRead = time.Now()
	r.mu.Unlock()
	r.trip()

	if tripped.Load() {
		t.Error("刚读到数据就被判空闲超时（边界竞态未修）")
	}
	r.mu.Lock()
	fired := r.fired
	r.mu.Unlock()
	if fired {
		t.Error("误判的 trip 不应把 fired 置真，否则后续 Read 不再续期")
	}
}

func TestIdleTripFiresWhenTrulyIdle(t *testing.T) {
	var tripped atomic.Bool
	r := idleReaderFor(t, 20*time.Millisecond, func() { tripped.Store(true) })
	defer r.Close()

	// 上次读取已经超过 timeout：这次是真空闲
	r.mu.Lock()
	r.lastRead = time.Now().Add(-time.Second)
	r.mu.Unlock()
	r.trip()

	if !tripped.Load() {
		t.Error("真空闲时应触发 onIdle")
	}
}

func TestIdleTripAfterCloseDoesNotFire(t *testing.T) {
	// 关闭后已派发的 trip 不该再触发，否则会给正常结束的流记一条假空闲超时
	var tripped atomic.Bool
	r := idleReaderFor(t, 20*time.Millisecond, func() { tripped.Store(true) })
	r.Close()
	r.mu.Lock()
	r.lastRead = time.Now().Add(-time.Second)
	r.mu.Unlock()
	r.trip()
	if tripped.Load() {
		t.Error("Close 之后不应再触发空闲超时")
	}
}

func TestIdleReaderResetsOnRead(t *testing.T) {
	// 持续吐字节的长流不该被空闲超时掐断（真实路径，非白盒）
	var tripped atomic.Bool
	src := io.NopCloser(&tickReader{chunks: 6, interval: 10 * time.Millisecond})
	rc := newIdleTimeoutReader(src, 40*time.Millisecond, func() { tripped.Store(true) })
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if tripped.Load() {
		t.Error("持续有数据的流被误判为空闲")
	}
}

// tickReader 每隔 interval 吐 1 字节，共 chunks 次，然后 EOF。
type tickReader struct {
	chunks   int
	interval time.Duration
}

func (r *tickReader) Read(p []byte) (int, error) {
	if r.chunks <= 0 {
		return 0, io.EOF
	}
	time.Sleep(r.interval)
	r.chunks--
	p[0] = 'x'
	return 1, nil
}
