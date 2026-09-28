package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// pushAll 把整段 Messages SSE 喂给转换器并收尾，返回完整 Responses SSE。
func pushAll(t *testing.T, model, text string) string {
	t.Helper()
	return pushAllCustom(t, model, text, nil)
}

// pushAllCustom 同 pushAll，额外指定 freeform（custom）工具名集合。
func pushAllCustom(t *testing.T, model, text string, customTools map[string]bool) string {
	t.Helper()
	tr := newMessagesSSETransformer(model, customTools)
	return tr.Push(text) + tr.Flush()
}

func TestParseSSEFrames(t *testing.T) {
	text := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: ping\ndata: {}\n\n"
	frames := parseSSEFrames(text)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[0].event != "message_start" || !strings.Contains(frames[0].data, "message_start") {
		t.Errorf("frames[0] = %+v", frames[0])
	}
	// 多行 data 拼接
	multi := parseSSEFrames("data: line1\ndata: line2\n\n")
	if len(multi) != 1 || multi[0].data != "line1\nline2" {
		t.Errorf("multi-line data = %+v", multi)
	}
}

func TestLastSSEBoundary(t *testing.T) {
	if got := lastSSEBoundary("data: a\n\n"); got != 9 {
		t.Errorf("lf boundary = %d, want 9", got)
	}
	if got := lastSSEBoundary("data: a\r\n\r\n"); got != 11 {
		t.Errorf("crlf boundary = %d, want 11", got)
	}
	if got := lastSSEBoundary("data: a\n\ndata: b"); got != 9 {
		t.Errorf("with pending = %d, want 9", got)
	}
	if got := lastSSEBoundary("data: a"); got != -1 {
		t.Errorf("no boundary = %d, want -1", got)
	}
}

func TestMessagesSSEThinkMarkerSplitAcrossFramesNotLeaked(t *testing.T) {
	// <think>…</think> 被切在多个 text_delta 帧里：任何一帧都不应把标记片段或思考正文
	// 下发给客户端，收尾正文应为思考块被剥离后的干净文本。
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello <thi"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"nk>secret rea"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"soning</thi"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"nk> world"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"

	out := pushAll(t, "m", sse)
	for _, leak := range []string{"<thi", "<think", "</think", "secret", "reasoning"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked internal marker/thinking %q in stream:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, `"text":"Hello  world"`) {
		t.Errorf("final sanitized text missing: %s", out)
	}
}

// TestMessagesSSEUnclosedThinkNotDropped 验证 text 块里 <thinking> 未闭合（思考被截断或漏打
// 闭合标签）时，正文不得从 <thinking> 起整体丢弃——否则这一轮会变成空回答提前收尾。
// 回归：finalizeSanitize 从最后一个未闭合 <think 起把尾部内容全部丢弃，答案信息丢失。
func TestMessagesSSEUnclosedThinkNotDropped(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello <thinking>secret answer"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"

	out := pushAll(t, "m", sse)
	if !strings.Contains(out, `"text":"Hello secret answer"`) {
		t.Errorf("unclosed think must keep content after stripping tag, got:\n%s", out)
	}
	if strings.Contains(out, "<thinking") {
		t.Errorf("think tag leaked into output:\n%s", out)
	}
}

func TestMessagesSSETextFlow(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"id":"msg_1","model":"claude sonnet 5"}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`, ``,
		`event: content_block_stop`, `data: {"type":"content_block_stop","index":0}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"

	out := pushAll(t, "claude sonnet 5", sse)
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		`"delta":"Hello"`,
		`"delta":" world"`,
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// message_start 应回填 model
	if !strings.Contains(out, `"model":"claude sonnet 5"`) {
		t.Errorf("model not backfilled: %s", out)
	}
	// 收尾 output_item.done 里应含完整文本
	if !strings.Contains(out, `"text":"Hello world"`) {
		t.Errorf("final text not assembled: %s", out)
	}
}

func TestMessagesSSEToolUseFlow(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude sonnet 5"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"

	out := pushAll(t, "claude sonnet 5", sse)
	for _, want := range []string{
		`"type":"function_call"`,
		`"name":"read_file"`,
		`"call_id":"toolu_1"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.completed",
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// arguments 由 input_json_delta 累积拼出
	if !strings.Contains(out, `{\"path\":\"a.go\"}`) {
		t.Errorf("arguments not accumulated: %s", out)
	}
}

func TestMessagesSSEDroppedToolFailsRound(t *testing.T) {
	// 上游声明 tool_use 但 name 缺失：message_stop 时不得谎报 completed，应 failed。
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"

	out := pushAll(t, "m", sse)
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "upstream_tool_call_dropped") {
		t.Fatalf("dropped tool use should fail:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("dropped tool use must not complete:\n%s", out)
	}
}

func TestMessagesSSENoMessageStopEmptyOutputFails(t *testing.T) {
	// 只到了 message_start 流就结束、一个内容块都没有：按 stream_truncated 报 failed，
	// 不能留一条只有 [DONE]、没有终态帧的断尾流。
	sse := `data: {"type":"message_start","message":{"id":"m1","model":"m"}}` + "\n\n"
	out := pushAll(t, "m", sse)
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_truncated") {
		t.Fatalf("无 message_stop 的空输出流应 failed/stream_truncated:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("不得谎报 completed:\n%s", out)
	}
}

func TestMessagesSSENoMessageStopWithDroppedToolFails(t *testing.T) {
	// 声明了 tool_use 但没有合法 name，且流在 message_stop 之前结束：
	// 口径与收到 message_stop 时一致，按 upstream_tool_call_dropped 报 failed。
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "upstream_tool_call_dropped") {
		t.Fatalf("丢弃工具调用 + 无 message_stop 应 failed/upstream_tool_call_dropped:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("不得谎报 completed:\n%s", out)
	}
}

func TestMessagesSSEWhitespaceToolNameFailsRound(t *testing.T) {
	// name 是纯空白：与缺失 name 同口径，不得当作可执行调用下发。
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"   "}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"

	out := pushAll(t, "m", sse)
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "upstream_tool_call_dropped") {
		t.Fatalf("whitespace tool name should fail:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("whitespace tool name must not complete:\n%s", out)
	}
}

func TestMessagesSSEDroppedToolWithUsableCallCompletes(t *testing.T) {
	// 同回合既有合法 tool_use 又有被丢弃的：只要下发过可执行调用就不该判失败。
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ok","name":"read_file"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_bad"}}`, ``,
		`data: {"type":"content_block_stop","index":1}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"

	out := pushAll(t, "m", sse)
	if strings.Contains(out, "response.failed") {
		t.Fatalf("round with a usable tool use must not fail:\n%s", out)
	}
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("expected response.completed:\n%s", out)
	}
	if !strings.Contains(out, `"call_id":"toolu_ok"`) {
		t.Errorf("usable tool use not emitted:\n%s", out)
	}
}

func TestMessagesSSEMixedToolAndTextUsesDenseIndexes(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`, ``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}`, ``,
		`data: {"type":"content_block_stop","index":1}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)
	if !strings.Contains(out, `"output_index":0,"type":"response.output_item.added"`) {
		t.Errorf("tool output index 0 missing: %s", out)
	}
	if !strings.Contains(out, `"output_index":1,"type":"response.output_item.added"`) {
		t.Errorf("message output index 1 missing: %s", out)
	}
	if strings.Contains(out, `"delta":"done","output_index":0`) {
		t.Errorf("text reused tool output index: %s", out)
	}
}

func TestMessagesSSECarriesUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1200}}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, ``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":340}}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)
	// response.completed 必须带上累积的 usage，否则客户端 token 计数恒为 0
	for _, want := range []string{`"input_tokens":1200`, `"output_tokens":340`, `"total_tokens":1540`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestMessagesSSEOmitsUsageWhenAbsent(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)
	// 上游没给 usage 时不应臆造 0 值字段
	if strings.Contains(out, `"usage"`) {
		t.Errorf("usage fabricated when upstream sent none:\n%s", out)
	}
}

func TestMessagesSSEChunkSplitAcrossBoundary(t *testing.T) {
	// 把一个事件从中间切断，验证跨 chunk 状态保持
	full := `data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}` + "\n\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"
	split := len(full) / 2

	tr := newMessagesSSETransformer("m", nil)
	out := tr.Push(full[:split])
	out += tr.Push(full[split:])
	out += tr.Flush()

	if !strings.Contains(out, "event: response.created") {
		t.Errorf("missing response.created across chunk split:\n%s", out)
	}
	if !strings.Contains(out, `"delta":"hi"`) {
		t.Errorf("missing text delta across chunk split:\n%s", out)
	}
}

func TestMessagesSSEIncompleteEventHeldPending(t *testing.T) {
	tr := newMessagesSSETransformer("m", nil)
	// 无完整边界 → 全部 pending，不输出
	if out := tr.Push(`data: {"type":"message_start"`); out != "" {
		t.Errorf("incomplete event produced output: %q", out)
	}
	// 补齐后输出
	out := tr.Push(`,"message":{"id":"m1"}}` + "\n\n")
	if !strings.Contains(out, "event: response.created") {
		t.Errorf("completed event not emitted: %q", out)
	}
}

func TestMessagesSSEError(t *testing.T) {
	sse := `data: {"type":"error","error":{"type":"overloaded_error","message":"boom"}}` + "\n\n"
	out := pushAll(t, "m", sse)
	if !strings.Contains(out, "event: response.failed") {
		t.Errorf("missing response.failed: %s", out)
	}
	if !strings.Contains(out, "overloaded_error") || !strings.Contains(out, "data: [DONE]") {
		t.Errorf("error payload/DONE missing: %s", out)
	}
}

func TestMessagesSSEDoneSwallowed(t *testing.T) {
	// 上游的 [DONE] 一律吞掉、不原样转发：否则客户端在首个 [DONE] 处停读，
	// synthesizeTerminal 补的终态帧永远到不了它。终态帧由 Flush 统一补。
	out := pushAll(t, "m", "data: [DONE]\n\n")
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_truncated") {
		t.Errorf("裸 [DONE] 应由 synthesizeTerminal 补 response.failed(stream_truncated):\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("应以 [DONE] 收尾:\n%s", out)
	}
}

func TestMessagesSSECustomToolCallFlow(t *testing.T) {
	// 上游按单字符串参数吐 {"input":"<patch>"}，回程要还原成 custom_tool_call 的顶层 input
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude sonnet 5"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"apply_patch"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"*** Begin Patch\\n"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"*** End Patch\"}"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"

	out := pushAllCustom(t, "claude sonnet 5", sse, map[string]bool{"apply_patch": true})
	for _, want := range []string{
		`"type":"custom_tool_call"`,
		`"name":"apply_patch"`,
		`"call_id":"toolu_1"`,
		"event: response.custom_tool_call_input.done",
		"event: response.output_item.done",
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// patch 必须是拆包后的纯文本（转义形态出现在 JSON 串里）
	if !strings.Contains(out, `*** Begin Patch\n*** End Patch`) {
		t.Errorf("input 未拆包成纯文本:\n%s", out)
	}
	// 不能把 {"input":...} 的 JSON 骨架当 arguments 增量泄漏给客户端
	if strings.Contains(out, "response.function_call_arguments") {
		t.Errorf("custom 工具误发了 function_call_arguments 事件:\n%s", out)
	}
	if strings.Contains(out, `"type":"function_call"`) {
		t.Errorf("custom 工具误发成 function_call:\n%s", out)
	}
}

func TestMessagesSSEFunctionCallUnaffectedByCustomSet(t *testing.T) {
	// 声明了 custom 工具，但本次调用的是普通 function：仍走 arguments 事件
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_2","name":"exec_command"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	out := pushAllCustom(t, "m", sse, map[string]bool{"apply_patch": true})
	if !strings.Contains(out, `"type":"function_call"`) {
		t.Errorf("普通 function 工具被误判成 custom:\n%s", out)
	}
	for _, want := range []string{
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "custom_tool_call") {
		t.Errorf("普通工具误发 custom_tool_call:\n%s", out)
	}
}

func TestMessagesSSESanitizesMinimaxLeak(t *testing.T) {
	sse := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok<]minimax[>["}}` + "\n\n"
	out := pushAll(t, "minimax-m3", sse)
	if strings.Contains(out, "minimax[>[") {
		t.Errorf("minimax leak marker not sanitized: %s", out)
	}
	if !strings.Contains(out, `"delta":"ok"`) {
		t.Errorf("sanitized delta wrong: %s", out)
	}
}

func TestMessagesSSEErrorIsTerminal(t *testing.T) {
	// A7 回归：上游在 error 之后仍发 message_stop 时，此前会先 failed 再 completed
	// + 第二个 [DONE]，客户端拿到自相矛盾的状态（先失败后完成）
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"boom"}}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)

	if !strings.Contains(out, "event: response.failed") {
		t.Fatalf("缺 response.failed:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Errorf("failed 之后又发了 completed:\n%s", out)
	}
	if n := strings.Count(out, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] 出现 %d 次，应只有 1 次:\n%s", n, out)
	}
}

func TestMessagesSSECompletedIsTerminal(t *testing.T) {
	// 重复的 message_stop 不应产出第二份 completed/[DONE]
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"message_stop"}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)
	// 数 event: 行，避免把 data 里的 "type":"response.completed" 也算进去
	if n := strings.Count(out, "event: response.completed"); n != 1 {
		t.Errorf("response.completed 出现 %d 次，应只有 1 次:\n%s", n, out)
	}
	if n := strings.Count(out, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] 出现 %d 次，应只有 1 次:\n%s", n, out)
	}
}

func TestMessagesSSESanitizesMarkerSplitAcrossDeltas(t *testing.T) {
	// B2 回归护栏：累积文本改存原文后，跨帧被切断的 minimax 标记仍须由收尾那次 sanitize 清掉
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"minimax-m3"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello<]mini"}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"max[>[ world"}}`, ``,
		`data: {"type":"content_block_stop","index":0}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "minimax-m3", sse)
	// content_part.done / output_item.done 里的完整文本不能残留标记
	if strings.Contains(out, "minimax[>[") {
		t.Errorf("跨帧标记未在收尾时清掉:\n%s", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("清理后正文应拼成 hello world:\n%s", out)
	}
}

// TestMessagesSSECreatedAtIsUnixSeconds 验证 response.created 的 created_at 是 Unix 秒
// （而非毫秒——此前误用 UnixMilli，客户端按秒解析会得到公元 58221 年）。
func TestMessagesSSECreatedAtIsUnixSeconds(t *testing.T) {
	tr := newMessagesSSETransformer("m", nil)
	out := tr.Push(`data: {"type":"message_start","message":{"id":"m1","model":"m"}}` + "\n\n")
	frame := extractFrameData(t, out, "response.created")
	var doc struct {
		Response struct {
			CreatedAt int64 `json:"created_at"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(frame), &doc); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if doc.Response.CreatedAt >= 1_000_000_000_000 { // 1e12：毫秒是 ~1.79e12，秒是 ~1.79e9
		t.Errorf("created_at = %d, want Unix 秒（<1e12，当前是毫秒）", doc.Response.CreatedAt)
	}
	if doc.Response.CreatedAt < 1_600_000_000 {
		t.Errorf("created_at = %d, 太早（不在近年的 Unix 秒范围）", doc.Response.CreatedAt)
	}
}

func TestMessagesSSETerminalBeforeForwardedDone(t *testing.T) {
	// 上游发 message_start + text_delta + [DONE] 但没 message_stop：终态帧必须出现在客户端
	// 读到的 [DONE] 之前，否则客户端在 [DONE] 停读、看不到终态（断尾流未被修复）。
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, ``,
		`data: [DONE]`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)
	if !strings.Contains(out, "response.incomplete") && !strings.Contains(out, "response.failed") {
		t.Fatalf("截断流应补终态帧:\n%s", out)
	}
	doneIdx := strings.Index(out, "data: [DONE]")
	termIdx := strings.Index(out, "response.incomplete")
	if termIdx < 0 {
		termIdx = strings.Index(out, "response.failed")
	}
	if doneIdx < 0 || termIdx > doneIdx {
		t.Fatalf("终态帧必须在 [DONE] 之前: doneIdx=%d termIdx=%d\n%s", doneIdx, termIdx, out)
	}
}

func TestMessagesSSEBareDoneSynthesizesFailure(t *testing.T) {
	// 裸 [DONE]（全程无协议事件）按截断失败收尾：客户端拿不到 message_stop 会自行重试
	//（见 codex codex-rs/core/tests/suite/stream_no_completed.rs），不该记 200 成功。
	tr := newMessagesSSETransformer("m", nil)
	out := tr.Push("data: [DONE]\n\n") + tr.Flush()
	if !tr.ProtocolIncomplete() {
		t.Error("裸 [DONE] 流应算截断")
	}
	if !tr.TerminalFailed() {
		t.Error("裸 [DONE] 流应算失败终态")
	}
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_truncated") {
		t.Errorf("应补 response.failed(stream_truncated):\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("应以 [DONE] 收尾:\n%s", out)
	}
}

func TestMessagesSSEDroppedToolFinalizesMessageItem(t *testing.T) {
	// 有流式文本 + 无 name 的 tool_use + message_stop：报 failed 前应先收尾 message 项，
	// 不留 dangling in_progress。
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`, ``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"","input":{}}}`, ``,
		`data: {"type":"content_block_stop","index":1}`, ``,
		`data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)
	if !strings.Contains(out, "upstream_tool_call_dropped") {
		t.Fatalf("应报 upstream_tool_call_dropped:\n%s", out)
	}
	if !strings.Contains(out, "response.output_item.done") {
		t.Errorf("message 项应先收尾（output_item.done）:\n%s", out)
	}
}

func TestMessagesSSEOnlyUnclosedThinkingIsTruncated(t *testing.T) {
	// 只有未闭合 <thinking>（清洗后为空、零交付）+ 无 message_stop：应报 stream_truncated
	// 而非 response.incomplete（后者会让 codex 以为有产出）。
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`, ``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<thinking>"}}`, ``,
	}, "\n") + "\n"
	out := pushAll(t, "m", sse)
	if !strings.Contains(out, "stream_truncated") {
		t.Fatalf("零交付的未闭合 thinking 截断应报 stream_truncated:\n%s", out)
	}
	if strings.Contains(out, "response.incomplete") {
		t.Errorf("不应报 response.incomplete:\n%s", out)
	}
}

func TestMessagesSSEBufferingThinkDuringUnclosedThink(t *testing.T) {
	// 未闭合 <thinking> 缓冲期（对下游零输出）应报告 BufferingThink=true，供流循环发保活帧。
	tr := newMessagesSSETransformer("m", nil)
	tr.Push(`data: {"type":"message_start","message":{"id":"m1","model":"m"}}` + "\n\n")
	tr.Push(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
	tr.Push(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<thinking>still going"}}` + "\n\n")
	if !tr.BufferingThink() {
		t.Error("未闭合 <thinking> 缓冲期应报告 BufferingThink=true")
	}
}
