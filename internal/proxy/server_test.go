package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer 构造不依赖宿主环境的代理服务。
func newTestServer(t *testing.T, cfg *Config) *Server {
	t.Helper()
	clearProxyEnv(t)
	s, err := NewServer(cfg, NewLogger(io.Discard, false))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func TestServerDashboardRoute(t *testing.T) {
	s := newTestServer(t, testConfig("http://127.0.0.1:1"))
	for _, path := range []string{"/", "/dashboard"} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("%s: code = %d, want 200", path, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q, want text/html", path, ct)
		}
		body := rr.Body.String()
		// 页面骨架 + 轮询逻辑必须在（这是唯一没有单测覆盖的产物，容易改坏）
		for _, want := range []string{
			"<!doctype html>", `id="cards"`, `id="eventRows"`, `id="sourceRows"`,
			`fetch("/api/stats"`, "setInterval(load",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: dashboard missing %q", path, want)
			}
		}
		// 前端读取的字段名必须与 Stats 的 JSON tag 对齐
		for _, field := range []string{"upstreamBase", "uptimeSec", "retriesUpstream", "recentEvents", "sources"} {
			if !strings.Contains(body, field) {
				t.Errorf("%s: dashboard does not reference stats field %q", path, field)
			}
		}
	}
}

func TestServerStatsRoute(t *testing.T) {
	s := newTestServer(t, testConfig("http://127.0.0.1:1"))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest("GET", "/api/stats", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var stats map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
		t.Fatalf("stats not valid JSON: %v", err)
	}
	// 仪表盘依赖的字段必须存在（改 Stats 结构时这里会挡住）
	for _, k := range []string{"ok", "now", "upstreamBase", "uptimeSec", "requests", "responses",
		"retriesUpstream", "retries429", "errors", "non200", "recentEvents", "sources"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("stats missing field %q", k)
		}
	}
}

func TestServerHealthzRoute(t *testing.T) {
	s := newTestServer(t, testConfig("http://127.0.0.1:1"))
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var h map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatalf("healthz not valid JSON: %v", err)
	}
	for _, k := range []string{"ok", "upstreamBase", "maxAttempts", "retryableStatuses",
		"streamIdleTimeoutMs", "artifactRetentionH", "logRotation", "headroomLite", "reasoningOnlyRetry"} {
		if _, ok := h[k]; !ok {
			t.Errorf("healthz missing field %q", k)
		}
	}
}

func TestServerProbeRoutesNotForwarded(t *testing.T) {
	s := newTestServer(t, testConfig("http://127.0.0.1:1"))
	// 浏览器/DevTools 探测请求必须本地 204，不能去打上游（上游地址是黑洞端口，转发会超时/报错）
	for _, path := range []string{"/favicon.ico", "/.well-known/appspecific/com.chrome.devtools.json"} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusNoContent {
			t.Errorf("%s: code = %d, want 204", path, rr.Code)
		}
	}
}

func TestServerHeadroomOriginalRouteRejectsBadHash(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1")
	cfg.Headroom = HeadroomConfig{Mode: "on", Apply: true, StoreDir: t.TempDir(), MinChars: 512}
	s := newTestServer(t, cfg)
	// 非 64 位 hex 一律 404（防路径穿越）
	for _, hash := range []string{"zzz", "../../etc/passwd", strings.Repeat("g", 64)} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest("GET", "/headroom-lite/"+hash, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("hash %q: code = %d, want 404", hash, rr.Code)
		}
	}
}
