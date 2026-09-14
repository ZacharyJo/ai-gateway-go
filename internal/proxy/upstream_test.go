package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

)

// testConfig 构造一个用于测试的配置：小延迟重试参数，冷却关闭。
func testConfig(upstreamBase string) *Config {
	return &Config{
		UpstreamBase:        upstreamBase,
		MaxAttempts:         3,
		RetryDelayMs:        5,
		MaxRetryDelayMs:     10,
		RetryStepDelayMs:    5,
		RateLimitCooldownMs: 0,
		RetryableStatuses:   map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true},
	}
}

// newTestUpstream 构造不依赖宿主环境的转发器（登录态替换为 nil，避免打真实接口）。
func newTestUpstream(t *testing.T, cfg *Config) *Upstream {
	t.Helper()
	clearProxyEnv(t)
	up, err := NewUpstream(cfg, NewLogger(io.Discard, false), nil, nil)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	return up
}

func TestUpstreamPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/v1/responses", "/responses"},
		{"/v1", "/"},
		{"/v1/", "/"},
		{"/v1/chat/completions", "/chat/completions"},
		{"/other", "/other"},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := upstreamPath(c.in); got != c.want {
			t.Errorf("upstreamPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestForwardSuccess(t *testing.T) {
	clearProxyEnv(t)
	var gotAccept, gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept-Encoding")
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("X-Request-ID", "abc")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer token123")
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", res.Status)
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if rr.Code != http.StatusOK {
		t.Errorf("rr.Code = %d, want 200", rr.Code)
	}
	if rr.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q, want forwarded body", rr.Body.String())
	}
	if gotAccept != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity", gotAccept)
	}
	if gotAuthorization != "Bearer token123" {
		t.Errorf("Authorization = %q, want passthrough", gotAuthorization)
	}
	if rr.Header().Get("X-Request-ID") != "abc" {
		t.Errorf("response header not forwarded: %q", rr.Header().Get("X-Request-ID"))
	}
}

func TestForwardRetryThenSuccess(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("boom"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200 after retry", res.Status)
	}
	if res.Attempts != 2 || attempts.Load() != 2 {
		t.Errorf("attempts = %d (server %d), want 2", res.Attempts, attempts.Load())
	}
	if rr.Body.String() != "ok" {
		t.Errorf("body = %q, want retried body", rr.Body.String())
	}
}

func TestForwardTransient404(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200 (transient 404 retried)", res.Status)
	}
	if attempts.Load() != 2 {
		t.Errorf("server hits = %d, want 2", attempts.Load())
	}
}

func TestForwardExhaustedRetryable(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	// 用尽重试后交付最后一次上游响应（让客户端看到真实错误）
	if res.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429 (last upstream delivered)", res.Status)
	}
	if res.Attempts != 3 || attempts.Load() != 3 {
		t.Errorf("attempts = %d (server %d), want 3", res.Attempts, attempts.Load())
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("rr.Code = %d, want 429", rr.Code)
	}
}

func TestForwardNonRetryable404(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("GET", "/v1/whatever", nil)
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404 (non-retryable)", res.Status)
	}
	if attempts.Load() != 1 {
		t.Errorf("server hits = %d, want 1", attempts.Load())
	}
}

func TestForwardNetworkErrorExhausted(t *testing.T) {
	// 占一个端口后立即关闭，模拟连接拒绝
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()

	up := newTestUpstream(t, testConfig("http://"+addr))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503 (exhausted)", res.Status)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("rr.Code = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "upstream_unavailable") {
		t.Errorf("body = %q, want upstream_unavailable error", rr.Body.String())
	}
}

func TestForwardSSEAppendsDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 故意不给 [DONE]，验证流式透传后兜底补帧
		w.Write([]byte("data: {\"type\":\"response.output_text.delta\"}\n\n"))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", res.Status)
	}
	if !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("SSE body missing [DONE] fallback: %q", rr.Body.String())
	}
}

func TestForwardCapturesSSEErrorBody(t *testing.T) {
	clearProxyEnv(t)
	dir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("data: upstream failure\n\n"))
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.CaptureErrorBodies = true
	cfg.LogDir = dir
	up := newTestUpstream(t, cfg)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6"}`))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 7)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusBadGateway || rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d/%d, want 502", res.Status, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "upstream failure") {
		t.Errorf("SSE error body not forwarded: %q", rr.Body.String())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "ai-gateway-captures", "*status-502.json"))
	if len(files) != 1 {
		t.Fatalf("capture files = %v, want one SSE error capture", files)
	}
}

func TestForwardKeepsClientAuthorizationWhenPresent(t *testing.T) {
	clearProxyEnv(t)
	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6"}`))
	req.Header.Set("Authorization", "Bearer sk-client-own")
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if gotAuthorization != "Bearer sk-client-own" {
		t.Errorf("Authorization = %q, want client's own (not overridden)", gotAuthorization)
	}
}

func TestForwardPreservesUpstreamBasePath(t *testing.T) {
	clearProxyEnv(t)
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// 上游 base 自带 /v1（与线上默认 UPSTREAM_BASE 一致），必须拼接而非覆盖，
	// 否则客户端 /v1/responses 会打到上游 /responses 而全部 404。
	up := newTestUpstream(t, testConfig(upstream.URL+"/v1"))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6"}`))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses (base path preserved)", gotPath)
	}

	// base 无路径时也应正确（客户端 /v1 前缀已剥离）
	up2 := newTestUpstream(t, testConfig(upstream.URL))
	req2 := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6"}`))
	if _, err := up2.Forward(req2.Context(), httptest.NewRecorder(), req2, 2); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses (no base path)", gotPath)
	}
}

func TestForwardNativeMessagesStreamKeepsNoDone(t *testing.T) {
	clearProxyEnv(t)
	native := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(native))
	}))
	defer upstream.Close()

	// 客户端直连 /v1/messages（Claude Code 的原生端点）→ 纯透传，不能被补 [DONE]
	up := newTestUpstream(t, testConfig(upstream.URL))
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude sonnet 5","stream":true}`))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if rr.Body.String() != native {
		t.Errorf("native Anthropic stream modified:\n got %q\nwant %q", rr.Body.String(), native)
	}
	if strings.Contains(rr.Body.String(), "[DONE]") {
		t.Error("[DONE] injected into native /v1/messages passthrough")
	}
}

func TestIsAnthropicNativePath(t *testing.T) {
	for _, p := range []string{"/messages", "/messages/count_tokens"} {
		if !isAnthropicNativePath(p) {
			t.Errorf("isAnthropicNativePath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/responses", "/chat/completions", "/messagesx", "/"} {
		if isAnthropicNativePath(p) {
			t.Errorf("isAnthropicNativePath(%q) = true, want false", p)
		}
	}
}

func TestForwardAdapterNonStreaming(t *testing.T) {
	clearProxyEnv(t)
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_1","model":"claude sonnet 5","role":"assistant",
			"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	reqBody := `{"model":"claude sonnet 5","instructions":"be brief","input":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("Status = %d, want 200", res.Status)
	}
	// 上游收到 /messages 且是 Messages 请求体
	if gotPath != "/messages" {
		t.Errorf("upstream path = %q, want /messages", gotPath)
	}
	if gotBody["system"] != "be brief" {
		t.Errorf("upstream system = %v, want instructions", gotBody["system"])
	}
	if _, ok := gotBody["messages"].([]any); !ok {
		t.Errorf("upstream body missing messages: %v", gotBody)
	}
	// 客户端收到 Responses 格式
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("client body invalid JSON: %v (%s)", err, rr.Body.String())
	}
	if out["object"] != "response" || out["id"] != "resp_msg_1" {
		t.Errorf("client body = %v, want Responses shape", out)
	}
	item := out["output"].([]any)[0].(map[string]any)
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "hi" {
		t.Errorf("converted output = %v", content)
	}
}

func TestForwardAdapterStreaming(t *testing.T) {
	clearProxyEnv(t)
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude sonnet 5"}}` + "\n\n"))
		w.Write([]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"))
		w.Write([]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"))
		w.Write([]byte(`data: {"type":"message_stop"}` + "\n\n"))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	reqBody := `{"model":"claude sonnet 5","stream":true,"input":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if gotPath != "/messages" {
		t.Errorf("upstream path = %q, want /messages", gotPath)
	}
	body := rr.Body.String()
	// Messages SSE 已转成 Responses SSE
	for _, want := range []string{"event: response.created", `"delta":"hi"`, "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in converted SSE:\n%s", want, body)
		}
	}
	// 不应残留 Messages 事件名
	if strings.Contains(body, "message_start") || strings.Contains(body, "content_block_delta") {
		t.Errorf("raw Messages events leaked to client:\n%s", body)
	}
}

func TestForwardGptPassthroughNoAdapter(t *testing.T) {
	clearProxyEnv(t)
	var gotPath string
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	reqBody := `{"model":"gpt-5.6","input":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	// GPT 系原样透传：路径仍是 /responses，body 未改写
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses (passthrough)", gotPath)
	}
	if gotBody != reqBody {
		t.Errorf("body was modified for GPT model:\n got %s\nwant %s", gotBody, reqBody)
	}
	if rr.Body.String() != `{"ok":true}` {
		t.Errorf("response was converted for GPT model: %s", rr.Body.String())
	}
}

func TestForwardAdapterRequestError(t *testing.T) {
	clearProxyEnv(t)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	// 非 GPT 模型 + 远程图片 URL → 适配报错，本地 400，不打上游
	reqBody := `{"model":"claude sonnet 5","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want 400", res.Status)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream hit %d times, want 0 (fail before forwarding)", hits.Load())
	}
	if !strings.Contains(rr.Body.String(), "responses_messages_adapter_error") {
		t.Errorf("body = %s, want adapter error type", rr.Body.String())
	}
}

func TestForwardHeadroomUpdatesContentLength(t *testing.T) {
	clearProxyEnv(t)
	// on 模式 headroom（阈值放低，保证接受）
	cfgH := HeadroomConfig{
		Mode: "on", Apply: true, MinChars: 512, HeadChars: 2500, TailChars: 2500, MaxJSONItems: 15,
		StoreDir: t.TempDir(), KeepLineContext: 3, MaxSnippets: 20,
		MinSavedChars: 100, MinSavingsRatio: 0.2, MinSavedTokens: 0, MinTokenSavingsRatio: 0,
		LiveZonePolicy: "latest-per-type",
	}
	hr := newHeadroom(cfgH)

	var gotCL int64
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCL = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.Headroom = cfgH
	up := newTestUpstream(t, cfg)
	up.hr = hr

	big := strings.Repeat("some ordinary log line text here\n", 200)
	doc := map[string]any{
		"model": "gpt-5.6", // GPT 系 → 原样透传，不走 Messages 适配
		"input": []any{map[string]any{"type": "function_call_output", "output": big}},
	}
	body, _ := json.Marshal(doc)
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	// body 确实被压缩
	if !bytes.Contains(gotBody, []byte("[ai-gateway headroom:")) {
		t.Errorf("body not compressed: %.80s", gotBody)
	}
	// headroom 改写了 body，Content-Length 必须等于实际长度（回归：修掉旧值覆盖 bug）
	if gotCL != int64(len(gotBody)) {
		t.Errorf("upstream Content-Length = %d, actual body %d (must match after headroom rewrite)", gotCL, len(gotBody))
	}
	if gotCL == int64(len(body)) {
		t.Error("body was not modified by headroom, test is vacuous")
	}
}

func TestForwardAuthModeBearerInjectsAPIKey(t *testing.T) {
	clearProxyEnv(t)
	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.AuthMode = "bearer"
	cfg.APIKey = "sk-third-party-key"
	up := newTestUpstream(t, cfg)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6"}`))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if gotAuthorization != "Bearer sk-third-party-key" {
		t.Errorf("bearer 模式下 Authorization = %q, want Bearer sk-third-party-key", gotAuthorization)
	}
}

func TestForwardAuthModeBearerKeepsClientAuthorization(t *testing.T) {
	clearProxyEnv(t)
	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.AuthMode = "bearer"
	cfg.APIKey = "sk-proxy-key"
	up := newTestUpstream(t, cfg)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6"}`))
	req.Header.Set("Authorization", "Bearer sk-client-own")
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	// 客户端已带 Authorization，代理不应覆盖
	if gotAuthorization != "Bearer sk-client-own" {
		t.Errorf("bearer 模式下客户端 Authorization 被覆盖: %q", gotAuthorization)
	}
}

func TestForwardImageFallbackOnUnsupportedError(t *testing.T) {
	// 阶段 13 集成测试：上游返回 400 + "不支持图片"错误时，代理替换图片重试
	clearProxyEnv(t)
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			// 第一次：报 image 不支持
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"Model do not support image input.","type":"BadRequest","param":"image_url","code":"InvalidParameter"}}`))
			return
		}
		// 第二次：成功
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		b, _ := io.ReadAll(r.Body)
		w.Write(b) // 回显 body，方便断言
	}))
	defer upstream.Close()

	up := newTestUpstream(t, testConfig(upstream.URL))
	// 使用非 GPT 模型（走 Messages 适配），并携带图片块
	reqBody := `{"model":"claude sonnet 5","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,abc="}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()
	res, err := up.Forward(req.Context(), rr, req, 1)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200（图片降级后重试应成功）", res.Status)
	}
	if attempts.Load() != 2 {
		t.Errorf("server hits = %d, want 2（一次 400 + 一次重试）", attempts.Load())
	}
	// 重试请求的 body 不应再包含 image 类型块
	body := rr.Body.String()
	// 回显的是 Messages 格式，检查没有 image_url 字段（已被占位文本替换）
	if strings.Contains(body, `"image_url"`) || strings.Contains(body, "input_image") {
		t.Errorf("重试请求仍含图片块: %s", body)
	}
}

func TestForwardImageFallbackNotRetriggered(t *testing.T) {
	// 阶段 13：每次请求最多触发一次图片降级（imageRetried 防止无限循环）
	clearProxyEnv(t)
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		// 每次都报图片错误
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"Model do not support image input.","type":"BadRequest"}}`))
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.MaxAttempts = 1 // 禁用 HTTP 重试，避免掩盖图片重试
	up := newTestUpstream(t, cfg)
	reqBody := `{"model":"claude sonnet 5","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,abc="}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	// 图片降级一次（2 次请求），之后交付最后一次响应，不再继续
	if attempts.Load() != 2 {
		t.Errorf("server hits = %d, want 2（降级仅触发一次）", attempts.Load())
	}
	if rr.Code != http.StatusBadRequest {
		t.Errorf("rr.Code = %d, want 400（最终交付上游错误）", rr.Code)
	}
}
