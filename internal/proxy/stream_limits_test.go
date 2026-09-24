package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRetryableKindFromSSE(t *testing.T) {
	cases := []struct {
		name string
		sse  string
		want string
	}{
		{
			"concurrency limit",
			`data: {"type":"error","error":{"type":"server_error","message":"Concurrency limit exceeded for account abc, please retry later."}}` + "\n\n",
			"concurrency_limit",
		},
		{
			"model at capacity",
			`data: {"type":"response.failed","response":{"error":{"message":"The selected model is at capacity, please try a different model."}}}` + "\n\n",
			"model_capacity",
		},
		{
			"server_is_overloaded code",
			`data: {"type":"error","error":{"code":"server_is_overloaded","message":"upstream busy"}}` + "\n\n",
			"model_capacity",
		},
		{
			"servers overloaded phrase",
			`data: {"type":"error","error":{"message":"Our servers are currently overloaded, please try again later."}}` + "\n\n",
			"model_capacity",
		},
		{
			"fingerprint replay cooldown is terminal",
			`data: {"type":"error","error":{"code":"fingerprint_replay_cooldown","message":"相同 Prompt 指纹近期已收到上游，当前请求进入精确重放冷却"}}` + "\n\n",
			"fingerprint_cooldown",
		},
		// 只命中一半短语不算（避免误判正常错误）
		{
			"half phrase not enough",
			`data: {"type":"error","error":{"message":"concurrency limit exceeded for account abc"}}` + "\n\n",
			"",
		},
		// 正常内容里出现相似字样不应命中（不在 error 容器内）
		{
			"normal content not matched",
			`data: {"type":"response.output_text.delta","delta":"concurrency limit exceeded for account, please retry later"}` + "\n\n",
			"",
		},
		{"plain delta", `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n", ""},
		{"done", "data: [DONE]\n\n", ""},
	}
	for _, c := range cases {
		if got := retryableKindFromSSE(c.sse); got != c.want {
			t.Errorf("%s: kind = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStreamErrorDetectorAcrossChunks(t *testing.T) {
	d := &streamErrorDetector{}
	// 错误 JSON 被切成两个 chunk，拼起来才完整
	if kind := d.feed([]byte(`data: {"type":"error","error":{"message":"Concurrency limit exceeded`)); kind != "" {
		t.Errorf("partial chunk fired: %q", kind)
	}
	kind := d.feed([]byte(` for account abc, please retry later."}}` + "\n\n"))
	if kind != "concurrency_limit" {
		t.Errorf("joined chunks kind = %q, want concurrency_limit", kind)
	}
	// 命中后固定返回，不重复解析
	if again := d.feed([]byte("data: whatever\n\n")); again != "concurrency_limit" {
		t.Errorf("kind not sticky: %q", again)
	}
}

func TestStreamSSEWritesOverloadedFrameOnSoftError(t *testing.T) {
	upstreamSSE := `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" +
		`data: {"type":"error","error":{"message":"Concurrency limit exceeded for account abc, please retry later."}}` + "\n\n"
	rr := httptest.NewRecorder()
	kind, _ := streamSSE(rr, strings.NewReader(upstreamSSE), true)
	if kind != "concurrency_limit" {
		t.Fatalf("kind = %q, want concurrency_limit", kind)
	}
	body := rr.Body.String()
	// 已写出的内容保留 + 追加客户端认识的错误帧
	if !strings.Contains(body, `"delta":"partial"`) {
		t.Errorf("already-streamed content lost: %q", body)
	}
	if !strings.Contains(body, `"code":"server_overloaded"`) {
		t.Errorf("overloaded frame not written: %q", body)
	}
	// 命中后立即结束，不再补 [DONE]
	if strings.Contains(body, "[DONE]") {
		t.Errorf("[DONE] appended after soft error: %q", body)
	}
}

func TestStreamSSENoFrameOnNormalStream(t *testing.T) {
	normal := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" + "data: [DONE]\n\n"
	rr := httptest.NewRecorder()
	if kind, _ := streamSSE(rr, strings.NewReader(normal), true); kind != "" {
		t.Errorf("normal stream flagged: %q", kind)
	}
	if strings.Contains(rr.Body.String(), "server_overloaded") {
		t.Errorf("overloaded frame injected into normal stream: %q", rr.Body.String())
	}
}

func TestCollectErrorTextsOnlyInsideErrorContainers(t *testing.T) {
	var texts []string
	collectErrorTexts(map[string]any{
		"type":  "response.output_text.delta", // 顶层 type 不在 error 容器内 → 不采集
		"delta": "some text",
		"error": map[string]any{"message": "boom", "code": "x"},
	}, false, &texts)
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "boom") || !strings.Contains(joined, "x") {
		t.Errorf("error container texts missing: %v", texts)
	}
	if strings.Contains(joined, "response.output_text.delta") || strings.Contains(joined, "some text") {
		t.Errorf("collected text outside error container: %v", texts)
	}
}

func TestStreamSSETerminalCooldownNoOverloadedFrame(t *testing.T) {
	// 终态错误（指纹重放冷却）：不追加 server_overloaded 帧（否则诱导客户端重试撞冷却），
	// 但已随流转发的上游原始错误内容保留。
	upstreamSSE := `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" +
		`data: {"type":"error","error":{"code":"fingerprint_replay_cooldown","message":"进入精确重放冷却，剩余约 25 分钟"}}` + "\n\n"
	rr := httptest.NewRecorder()
	kind, _ := streamSSE(rr, strings.NewReader(upstreamSSE), true)
	if kind != "fingerprint_cooldown" {
		t.Fatalf("kind = %q, want fingerprint_cooldown", kind)
	}
	if !isTerminalStreamErrorKind(kind) {
		t.Errorf("kind %q should be classified terminal", kind)
	}
	body := rr.Body.String()
	if strings.Contains(body, "server_overloaded") {
		t.Errorf("terminal error must NOT inject server_overloaded (would trigger client retry): %q", body)
	}
	if !strings.Contains(body, "fingerprint_replay_cooldown") {
		t.Errorf("upstream terminal error content should pass through: %q", body)
	}
}

func TestStreamSSEChatSoftErrorStopsCleanly(t *testing.T) {
	// 上游把并发超限软错误塞进 200 的 Chat SSE 流。检测命中时：
	//   - 非终态：发 server_overloaded 帧 + [DONE] 干净收尾，**不**把原始 Chat chunk 透传
	//     进 Responses 流（那会把 Chat Completions JSON 混进 Responses 协议）；
	//   - 客户端拿到 server_overloaded 即重试，不需要看底层错误正文（终态错误才透传原始帧）。
	chatSSE := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"error":{"message":"Concurrency limit exceeded for account abc, please retry later."}}` + "\n\n"
	rr := httptest.NewRecorder()
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	kind, truncated := streamSSEChat(rr, strings.NewReader(chatSSE), tr)
	if kind != "concurrency_limit" {
		t.Fatalf("kind = %q, want concurrency_limit", kind)
	}
	if truncated {
		t.Error("soft error should not be flagged truncated")
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"code":"server_overloaded"`) {
		t.Errorf("overloaded frame not written: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("soft error should end with clean [DONE]: %q", body)
	}
	// 原始 Chat JSON（错误正文与同 chunk 的正常内容）不应混进 Responses 流。
	if strings.Contains(body, "Concurrency limit exceeded") || strings.Contains(body, `"content":"hi"`) {
		t.Errorf("raw Chat chunk leaked into Responses stream on soft error: %q", body)
	}
}
