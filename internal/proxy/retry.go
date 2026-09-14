package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// RetryPolicy 描述 HTTP 重试策略（指数退避算法）。
type RetryPolicy struct {
	MaxAttempts       int
	RetryDelayMs      int
	MaxRetryDelayMs   int
	RetryStepDelayMs  int
	RetryableStatuses map[int]bool
}

// ShouldRetry 判断状态码是否可重试：429/5xx，或 POST /responses 的 404（瞬态）。
func (p *RetryPolicy) ShouldRetry(status int, method, path string) bool {
	if p.RetryableStatuses[status] {
		return true
	}
	return status == http.StatusNotFound && method == http.MethodPost && path == "/responses"
}

// RetryAfterMs 计算第 attempt 次重试前的等待时间：
// 默认步进 RETRY_STEP_DELAY_MS*attempt，上限 MAX_RETRY_DELAY_MS；
// 上游 retry-after 头（秒数或 HTTP 日期）只让等待更短（三者取最小）。
func (p *RetryPolicy) RetryAfterMs(retryAfter string, attempt int, now time.Time) time.Duration {
	step := p.RetryDelayMs
	if p.RetryStepDelayMs > 0 {
		step = p.RetryStepDelayMs * attempt
	}
	if step > p.MaxRetryDelayMs {
		step = p.MaxRetryDelayMs
	}
	wait := time.Duration(step) * time.Millisecond
	if ra := parseRetryAfter(retryAfter, now); ra >= 0 && ra < wait {
		wait = ra
	}
	return wait
}

// parseRetryAfter 解析 retry-after 头：秒数或 HTTP 日期；无法解析返回 -1（忽略）。
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return -1
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Duration(max(0, secs)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
		return 0
	}
	return -1
}

// Cooldown 是进程级全局 429 冷却闸门。
// 上游返回 429 后，一段时间内所有新请求在发起上游调用前先等待，减少 429 风暴。
// RATE_LIMIT_COOLDOWN_MS=0 时整个机制关闭。
type Cooldown struct {
	cooldownMs int64        // 配置值（毫秒），0=关闭
	until      atomic.Int64 // 冷却截止的 unix 毫秒
}

// NewCooldown 构造冷却闸门。
func NewCooldown(rateLimitCooldownMs int) *Cooldown {
	return &Cooldown{cooldownMs: int64(rateLimitCooldownMs)}
}

// Enabled 返回冷却是否开启。
func (c *Cooldown) Enabled() bool { return c.cooldownMs > 0 }

// Wait 阻塞直到冷却结束（若在冷却中）。每次上游尝试前调用；ctx 取消时立即返回。
func (c *Cooldown) Wait(ctx context.Context) {
	for {
		wait := c.until.Load() - time.Now().UnixMilli()
		if wait <= 0 {
			return
		}
		timer := time.NewTimer(time.Duration(wait) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Extend 在 429 重试时延长冷却：until = max(until, now + max(cooldownMs, retryDelay))。
// cooldownMs 为 0 时不做事（机制关闭）。
func (c *Cooldown) Extend(retryDelay time.Duration) {
	if !c.Enabled() {
		return
	}
	delay := max(c.cooldownMs, int64(retryDelay/time.Millisecond))
	until := time.Now().UnixMilli() + delay
	// CAS 循环：避免 load-then-store 的竞态（两个 goroutine 都读到旧值，
	// 后写的那个可能覆盖掉更大的截止时间，导致冷却提前结束）。
	for {
		old := c.until.Load()
		if until <= old {
			break
		}
		if c.until.CompareAndSwap(old, until) {
			break
		}
	}
}

// sleepCtx 等待 d 时长；ctx 取消返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
