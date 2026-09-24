package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRetryPolicyShouldRetry(t *testing.T) {
	p := &RetryPolicy{RetryableStatuses: map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}}
	cases := []struct {
		status int
		method string
		path   string
		want   bool
	}{
		{429, "POST", "/responses", true},
		{500, "POST", "/responses", true},
		{502, "POST", "/responses", true},
		{503, "POST", "/responses", true},
		{504, "POST", "/responses", true},
		// POST /responses 的 404 是瞬态，重试
		{404, "POST", "/responses", true},
		// 其他 404 不重试
		{404, "GET", "/responses", false},
		{404, "POST", "/chat/completions", false},
		{200, "POST", "/responses", false},
		{400, "POST", "/responses", false},
	}
	for _, c := range cases {
		if got := p.ShouldRetry(c.status, c.method, c.path); got != c.want {
			t.Errorf("ShouldRetry(%d, %q, %q) = %v, want %v", c.status, c.method, c.path, got, c.want)
		}
	}
}

func TestRetryPolicyRetryAfterMs(t *testing.T) {
	p := &RetryPolicy{MaxAttempts: 5, RetryDelayMs: 2000, MaxRetryDelayMs: 4000, RetryStepDelayMs: 1000}
	now := time.Now()
	cases := []struct {
		name       string
		retryAfter string
		attempt    int
		want       time.Duration
	}{
		{"no header attempt 1", "", 1, 1000 * time.Millisecond},
		{"no header attempt 2", "", 2, 2000 * time.Millisecond},
		{"no header attempt 5 capped", "", 5, 4000 * time.Millisecond}, // step=5000 > max
		{"retry-after shorter than backoff -> keep backoff", "1", 2, 2000 * time.Millisecond},
		{"retry-after longer honored (min-wait semantics)", "10", 1, 10 * time.Second},
		{"retry-after huge capped at 60s", "600", 1, 60 * time.Second},
		{"retry-after date past -> 0", now.UTC().Add(-time.Hour).Format(http.TimeFormat), 2, 2000 * time.Millisecond},
		{"garbage ignored", "abc", 2, 2000 * time.Millisecond},
	}
	for _, c := range cases {
		if got := p.RetryAfterMs(c.retryAfter, c.attempt, now); got != c.want {
			t.Errorf("%s: RetryAfterMs = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Now()
	if got := parseRetryAfter("", now); got != -1 {
		t.Errorf("empty = %v, want -1", got)
	}
	if got := parseRetryAfter("abc", now); got != -1 {
		t.Errorf("garbage = %v, want -1", got)
	}
	if got := parseRetryAfter("30", now); got != 30*time.Second {
		t.Errorf("seconds = %v, want 30s", got)
	}
	if got := parseRetryAfter("-5", now); got != 0 {
		t.Errorf("negative seconds = %v, want 0", got)
	}
}

func TestCooldownEnabled(t *testing.T) {
	if c := NewCooldown(0); c.Enabled() {
		t.Error("Cooldown(0).Enabled() = true, want false")
	}
	if c := NewCooldown(2500); !c.Enabled() {
		t.Error("Cooldown(2500).Enabled() = false, want true")
	}
}

func TestCooldownExtend(t *testing.T) {
	// 冷却 100ms，重试延迟 10ms → 取 max(100, 10)=100ms
	c := NewCooldown(100)
	c.Extend(10 * time.Millisecond)
	remaining := c.until.Load() - time.Now().UnixMilli()
	if remaining < 90 || remaining > 110 {
		t.Errorf("remaining = %dms, want ~100ms", remaining)
	}

	// 重试延迟更大时取重试延迟：冷却 100ms，重试延迟 200ms → 200ms
	c2 := NewCooldown(100)
	c2.Extend(200 * time.Millisecond)
	remaining = c2.until.Load() - time.Now().UnixMilli()
	if remaining < 190 || remaining > 210 {
		t.Errorf("remaining = %dms, want ~200ms", remaining)
	}
}

func TestCooldownDisabledExtend(t *testing.T) {
	// 冷却关闭时 Extend 不做事
	c := NewCooldown(0)
	c.Extend(100 * time.Millisecond)
	if c.until.Load() != 0 {
		t.Errorf("until = %d, want 0 (disabled)", c.until.Load())
	}
}

func TestCooldownWait(t *testing.T) {
	c := NewCooldown(100)
	c.Extend(10 * time.Millisecond) // until = now+100ms
	start := time.Now()
	c.Wait(context.Background())
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("Wait returned after %v, want >= 90ms", elapsed)
	}

	// 未冷却时立即返回
	c2 := NewCooldown(100)
	start = time.Now()
	c2.Wait(context.Background())
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("idle Wait took %v, want immediate", elapsed)
	}
}

func TestCooldownWaitCancelled(t *testing.T) {
	c := NewCooldown(1000)
	c.Extend(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	c.Wait(ctx)
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("cancelled Wait took %v, want immediate", elapsed)
	}
}
