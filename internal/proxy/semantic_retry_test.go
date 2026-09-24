package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestModelMatchesReasoningRetry(t *testing.T) {
	patterns := []string{"gpt-5.6"}
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-5.6", true},
		{"gpt-5.6-terra", true}, // 前缀 + "-" 命中（用户实际模型）
		{"GPT-5.6-Terra", true}, // 大小写无关
		{"  gpt-5.6  ", true},
		{"gpt-5.6terra", false}, // 没有连字符分隔 → 不算
		{"gpt-5.7", false},
		{"claude sonnet 5", false},
		{"", false},
	}
	for _, c := range cases {
		if got := modelMatchesReasoningRetry(c.model, patterns); got != c.want {
			t.Errorf("modelMatchesReasoningRetry(%q) = %v, want %v", c.model, got, c.want)
		}
	}
	// 多 pattern
	if !modelMatchesReasoningRetry("glm-5.1", []string{"gpt-5.6", "glm-5.1"}) {
		t.Error("multi-pattern match failed")
	}
}

func TestHasPendingToolOutput(t *testing.T) {
	yes := map[string]any{"input": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "x"},
	}}
	if !hasPendingToolOutput(yes) {
		t.Error("function_call_output as last item should count as pending")
	}
	custom := map[string]any{"input": []any{map[string]any{"type": "custom_tool_call_output", "output": "x"}}}
	if !hasPendingToolOutput(custom) {
		t.Error("custom_tool_call_output should count as pending")
	}
	// 工具输出不在末位 → 不算
	no := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "output": "x"},
		map[string]any{"role": "user", "content": "继续"},
	}}
	if hasPendingToolOutput(no) {
		t.Error("tool output not last should not count")
	}
	if hasPendingToolOutput(map[string]any{"input": []any{}}) {
		t.Error("empty input should not count")
	}
	if hasPendingToolOutput(map[string]any{}) {
		t.Error("missing input should not count")
	}
}

func TestSummarizeResponsesSSE(t *testing.T) {
	// 只有 reasoning + completed（无可执行输出）→ 空转
	reasoningOnly := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}`, ``,
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"reasoning","id":"rs_1"}]}}`, ``,
		"data: [DONE]", ``,
	}, "\n")
	s := summarizeResponsesSSE(reasoningOnly)
	if !s.completed || !s.terminal || s.actionable {
		t.Errorf("reasoning-only summary = %+v, want completed/terminal without actionable", s)
	}
	if !shouldRetryReasoningOnly(s) {
		t.Error("reasoning-only completion should trigger retry")
	}

	// 有 message 输出 → 正常
	withMessage := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`, ``,
		`data: {"type":"response.completed","response":{"id":"resp_1"}}`, ``,
	}, "\n")
	s = summarizeResponsesSSE(withMessage)
	if !s.actionable {
		t.Errorf("message output not detected as actionable: %+v", s)
	}
	if shouldRetryReasoningOnly(s) {
		t.Error("stream with message output should not retry")
	}

	// 有 function_call → 正常
	withCall := `data: {"type":"response.completed","response":{"output":[{"type":"function_call","name":"read_file"}]}}` + "\n\n"
	if s := summarizeResponsesSSE(withCall); !s.actionable || shouldRetryReasoningOnly(s) {
		t.Errorf("function_call output = %+v, want actionable", s)
	}

	// 流没正常收尾（无 completed/failed/incomplete）→ 也重打
	partial := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"
	s = summarizeResponsesSSE(partial)
	if s.terminal {
		t.Errorf("partial stream marked terminal: %+v", s)
	}
	if !shouldRetryReasoningOnly(s) {
		t.Error("non-terminal stream should trigger retry")
	}

	// failed 收尾 → terminal，不重打（错误交给客户端）
	failed := `data: {"type":"response.failed","response":{"error":{"message":"boom"}}}` + "\n\n"
	s = summarizeResponsesSSE(failed)
	if !s.terminal || s.completed {
		t.Errorf("failed summary = %+v, want terminal without completed", s)
	}
	if shouldRetryReasoningOnly(s) {
		t.Error("failed stream should not trigger reasoning retry")
	}
}

func TestPlanReasoningRetry(t *testing.T) {
	cfg := &Config{
		ReasoningOnlyRetryEnabled: true,
		ReasoningOnlyRetryModels:  []string{"gpt-5.6"},
		ReasoningOnlyRetryMax:     1,
	}
	eligibleBody := []byte(`{"model":"gpt-5.6-terra","input":[{"type":"function_call_output","call_id":"c1","output":"x"}]}`)

	if p := planReasoningRetry(cfg, "/responses", eligibleBody); !p.eligible || p.model != "gpt-5.6-terra" {
		t.Errorf("eligible request = %+v, want eligible", p)
	}
	// 路径不对
	if p := planReasoningRetry(cfg, "/chat/completions", eligibleBody); p.eligible {
		t.Error("non-/responses path should not be eligible")
	}
	// 模型不在名单
	other := []byte(`{"model":"claude sonnet 5","input":[{"type":"function_call_output","output":"x"}]}`)
	if p := planReasoningRetry(cfg, "/responses", other); p.eligible {
		t.Error("off-list model should not be eligible")
	}
	// 末项不是工具输出
	noPending := []byte(`{"model":"gpt-5.6-terra","input":[{"role":"user","content":"hi"}]}`)
	if p := planReasoningRetry(cfg, "/responses", noPending); p.eligible {
		t.Error("no pending tool output should not be eligible")
	}
	// 开关关闭 / 上限 0
	off := *cfg
	off.ReasoningOnlyRetryEnabled = false
	if p := planReasoningRetry(&off, "/responses", eligibleBody); p.eligible {
		t.Error("disabled should not be eligible")
	}
	zero := *cfg
	zero.ReasoningOnlyRetryMax = 0
	if p := planReasoningRetry(&zero, "/responses", eligibleBody); p.eligible {
		t.Error("max=0 should not be eligible")
	}
	// 非 JSON body
	if p := planReasoningRetry(cfg, "/responses", []byte("not json")); p.eligible {
		t.Error("non-JSON body should not be eligible")
	}
}

// reasoningTestConfig 是启用空转重试的测试配置。
func reasoningTestConfig(upstreamBase string) *Config {
	cfg := testConfig(upstreamBase)
	cfg.ReasoningOnlyRetryEnabled = true
	cfg.ReasoningOnlyRetryModels = []string{"gpt-5.6"}
	cfg.ReasoningOnlyRetryMax = 1
	cfg.ReasoningOnlyRetryBufferBytes = 1 << 20
	return cfg
}

func TestForwardReasoningOnlyRetriesOnce(t *testing.T) {
	var hits atomic.Int32
	reasoningOnly := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}`, ``,
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"reasoning"}]}}`, ``,
		"data: [DONE]", ``,
	}, "\n")
	good := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_1"}}`, ``,
		`data: {"type":"response.completed","response":{"id":"resp_2"}}`, ``,
		"data: [DONE]", ``,
	}, "\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if hits.Add(1) == 1 {
			w.Write([]byte(reasoningOnly)) // 第一次空转
			return
		}
		w.Write([]byte(good)) // 重打后正常
	}))
	defer upstream.Close()

	up := newTestUpstream(t, reasoningTestConfig(upstream.URL))
	body := `{"model":"gpt-5.6-terra","stream":true,"input":[{"type":"function_call_output","call_id":"c1","output":"x"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2 (one discarded + one delivered)", hits.Load())
	}
	out := rr.Body.String()
	// 客户端只应看到第二次（有 message 输出）的内容
	if !strings.Contains(out, `"id":"msg_1"`) {
		t.Errorf("delivered body missing retried content: %q", out)
	}
	if strings.Contains(out, `"id":"rs_1"`) {
		t.Errorf("discarded reasoning-only response leaked to client: %q", out)
	}
}

func TestForwardReasoningOnlyDeliversWhenActionable(t *testing.T) {
	var hits atomic.Int32
	good := `data: {"type":"response.completed","response":{"output":[{"type":"message"}]}}` + "\n\n" + "data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(good))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, reasoningTestConfig(upstream.URL))
	body := `{"model":"gpt-5.6-terra","stream":true,"input":[{"type":"function_call_output","call_id":"c1","output":"x"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (no retry needed)", hits.Load())
	}
	if !strings.Contains(rr.Body.String(), "response.completed") {
		t.Errorf("body not delivered: %q", rr.Body.String())
	}
}

func TestForwardReasoningOnlyNotEligibleStreamsDirectly(t *testing.T) {
	var hits atomic.Int32
	reasoningOnly := `data: {"type":"response.completed","response":{"output":[{"type":"reasoning"}]}}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(reasoningOnly))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, reasoningTestConfig(upstream.URL))
	// 末项不是工具输出 → 无资格 → 直接流式交付，不缓冲不重打
	body := `{"model":"gpt-5.6-terra","stream":true,"input":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (not eligible)", hits.Load())
	}
	if !strings.Contains(rr.Body.String(), "response.completed") {
		t.Errorf("body not delivered: %q", rr.Body.String())
	}
}

func TestForwardReasoningOnlyBufferExceededPassesThrough(t *testing.T) {
	var hits atomic.Int32
	big := `data: {"type":"response.completed","response":{"output":[{"type":"reasoning","pad":"` +
		strings.Repeat("x", 4096) + `"}]}}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(big))
	}))
	defer upstream.Close()

	cfg := reasoningTestConfig(upstream.URL)
	cfg.ReasoningOnlyRetryBufferBytes = 1024 // 小于响应体 → 判定不了
	up := newTestUpstream(t, cfg)
	body := `{"model":"gpt-5.6-terra","stream":true,"input":[{"type":"function_call_output","output":"x"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	// fail-open：不重打，交付已读部分
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (buffer exceeded → no retry)", hits.Load())
	}
	if rr.Body.Len() == 0 {
		t.Error("nothing delivered on buffer-exceeded path")
	}
}

func TestForwardReasoningOnlyDeliversRepairedTools(t *testing.T) {
	// 上游把 custom 工具降级成 function_call（tool_shape 要修的故障）
	degraded := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"exec","id":"fc_1","arguments":""}}`, ``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"input\":\"git di"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"ff\"}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"input\":\"git diff\"}"}`, ``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"exec","id":"fc_1","arguments":"{\"input\":\"git diff\"}"}}`, ``,
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"function_call","name":"exec","id":"fc_1","arguments":"{\"input\":\"git diff\"}"}]}}`, ``,
		"data: [DONE]", ``,
	}, "\n")
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(degraded))
	}))
	defer upstream.Close()

	up := newTestUpstream(t, reasoningTestConfig(upstream.URL))
	// 资格命中（gpt-5.6 + 末项待执行工具输出）且声明 custom 工具 exec。
	// eligible 交付必须走与流式路径相同的 tool_shape 修理，否则客户端收到降级的 function_call。
	body := `{"model":"gpt-5.6-terra","stream":true,"tools":[{"type":"custom","name":"exec"}],"input":[{"type":"function_call_output","call_id":"c1","output":"x"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	if _, err := up.Forward(req.Context(), rr, req, 1); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (actionable → no semantic retry)", hits.Load())
	}
	out := rr.Body.String()
	if !strings.Contains(out, `"custom_tool_call"`) {
		t.Errorf("eligible buffered delivery should repair degraded function_call:\n%s", out)
	}
	if strings.Contains(out, `"type":"function_call"`) {
		t.Errorf("degraded function_call not repaired in eligible delivery:\n%s", out)
	}
}
