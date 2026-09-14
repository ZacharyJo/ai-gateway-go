package proxy

import (
	"io"
	"sync"
	"time"
)

// 流式空闲超时：上游长时间一个字节都不发时取消该次上游请求，回收挂死连接。
// 注意是"空闲"而非"总时长"—— 长思考流可能持续数分钟但会持续吐字节，不能用总超时掐断
// （原实现暴露了 STREAM_IDLE_TIMEOUT_MS 但实现是死代码，这里真正接上）。

// idleTimeoutReader 包装上游响应体：每次成功读取重置计时器，超时触发 onIdle（取消上游请求）。
type idleTimeoutReader struct {
	rc      io.ReadCloser
	timeout time.Duration
	onIdle  func()

	mu       sync.Mutex
	timer    *time.Timer
	fired    bool
	closed   bool
	lastRead time.Time
}

// newIdleTimeoutReader 构造并立即开始计时。timeout <= 0 时直接返回原 body（不包装）。
func newIdleTimeoutReader(rc io.ReadCloser, timeout time.Duration, onIdle func()) io.ReadCloser {
	if timeout <= 0 || onIdle == nil {
		return rc
	}
	t := &idleTimeoutReader{rc: rc, timeout: timeout, onIdle: onIdle, lastRead: time.Now()}
	t.timer = time.AfterFunc(timeout, t.trip)
	return t
}

// trip 在空闲超时后取消上游请求，使后续 Read 立即返回错误。
func (t *idleTimeoutReader) trip() {
	t.mu.Lock()
	if t.fired || t.closed {
		t.mu.Unlock()
		return
	}
	// AfterFunc 一旦派发，Read 里的 timer.Reset 就拦不住它了：字节恰好在超时边界到达时
	// 这次触发是误判。按距上次读取的剩余时间重新排期，而不是取消一个还在吐数据的流。
	if remaining := t.timeout - time.Since(t.lastRead); remaining > 0 {
		if t.timer != nil {
			t.timer.Reset(remaining)
		}
		t.mu.Unlock()
		return
	}
	t.fired = true
	t.mu.Unlock()
	t.onIdle()
}

func (t *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	t.mu.Lock()
	// 已触发就不再续期：连接已被取消，让错误如实传出去
	if !t.fired && t.timer != nil {
		t.lastRead = time.Now()
		t.timer.Reset(t.timeout)
	}
	t.mu.Unlock()
	return n, err
}

func (t *idleTimeoutReader) Close() error {
	t.mu.Lock()
	t.closed = true // 关闭后已派发的 trip 不再触发，避免给正常结束的流记一条假空闲超时
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.mu.Unlock()
	return t.rc.Close()
}
