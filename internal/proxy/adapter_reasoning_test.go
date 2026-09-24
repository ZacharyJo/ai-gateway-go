package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// collectFrameData 收集所有指定 event 的 data 载荷（按出现顺序）。
func collectFrameData(sse, event string) []string {
	var out []string
	for _, f := range parseSSEFrames(sse) {
		if f.event == event {
			out = append(out, f.data)
		}
	}
	return out
}

// collectDeltaText 收集指定 event 的 data 里 "delta" 字段文本并拼接。
func collectDeltaText(sse, event string) string {
	var b strings.Builder
	for _, data := range collectFrameData(sse, event) {
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			continue
		}
		if d, ok := m["delta"].(string); ok {
			b.WriteString(d)
		}
	}
	return b.String()
}

// TestSplitLeadingThinkBlock 覆盖 <think> 前导块切分的边界。
func TestSplitLeadingThinkBlock(t *testing.T) {
	cases := []struct {
		name, in, wantR, wantA string
		wantOK                 bool
	}{
		{"basic", "<think>思考</think>正文", "思考", "正文", true},
		{"leading ws + newline sep", "  <think>abc</think>\n\nhello", "abc", "hello", true},
		{"no open tag", "just text", "", "", false},
		{"open no close", "<think>unterminated", "", "", false},
		{"empty think", "<think></think>answer", "", "answer", true},
	}
	for _, c := range cases {
		r, a, ok := splitLeadingThinkBlock(c.in)
		if ok != c.wantOK || r != c.wantR || a != c.wantA {
			t.Errorf("%s: got (%q,%q,%v), want (%q,%q,%v)", c.name, r, a, ok, c.wantR, c.wantA, c.wantOK)
		}
	}
}

// TestExtractReasoningText 覆盖多种 reasoning 字段形态。
func TestExtractReasoningText(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"reasoning_content string", map[string]any{"reasoning_content": "rc"}, "rc"},
		{"reasoning string", map[string]any{"reasoning": "rs"}, "rs"},
		{"reasoning object content", map[string]any{"reasoning": map[string]any{"content": "obj"}}, "obj"},
		{"reasoning_content strips think tags", map[string]any{"reasoning_content": "<think>rc</think>"}, "rc"},
		{"prefers reasoning_content", map[string]any{"reasoning_content": "rc", "reasoning": "rs"}, "rc"},
		{"empty ignored", map[string]any{"reasoning_content": "", "reasoning": "rs"}, "rs"},
		{"none", map[string]any{"content": "x"}, ""},
	}
	for _, c := range cases {
		if got := extractReasoningText(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestChatNonStreamReasoningFieldSeparated 验证非流式：reasoning_content 归独立 reasoning 项，
// 正文 content 归 output_text，二者不混。
func TestChatNonStreamReasoningFieldSeparated(t *testing.T) {
	doc := map[string]any{
		"id":    "c1",
		"model": "deepseek-v4-flash",
		"choices": []any{map[string]any{
			"index":         float64(0),
			"finish_reason": "stop",
			"message": map[string]any{
				"role":              "assistant",
				"content":           "最终答复",
				"reasoning_content": "我的思考过程",
			},
		}},
	}
	resp, err := chatCompletionToResponsesBody(doc, 1000, 1, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output len = %d, want 2 (reasoning + message):\n%+v", len(output), output)
	}
	first, _ := output[0].(map[string]any)
	if first["type"] != "reasoning" {
		t.Errorf("output[0].type = %v, want reasoning", first["type"])
	}
	summary, _ := first["summary"].([]any)
	sm, _ := summary[0].(map[string]any)
	if sm["text"] != "我的思考过程" {
		t.Errorf("reasoning text = %v, want 我的思考过程", sm["text"])
	}
	second, _ := output[1].(map[string]any)
	if second["type"] != "message" {
		t.Errorf("output[1].type = %v, want message", second["type"])
	}
	content, _ := second["content"].([]any)
	cm, _ := content[0].(map[string]any)
	if cm["text"] != "最终答复" {
		t.Errorf("message text = %v, want 最终答复 (思考不应混进正文)", cm["text"])
	}
}

// TestChatNonStreamInlineThinkStripped 验证非流式：正文里内嵌的 <think> 块被剥到 reasoning，
// 正文只保留 </think> 之后的部分。
func TestChatNonStreamInlineThinkStripped(t *testing.T) {
	doc := map[string]any{
		"id":    "c2",
		"model": "m",
		"choices": []any{map[string]any{
			"index":         float64(0),
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": "<think>内嵌思考</think>干净正文",
			},
		}},
	}
	resp, err := chatCompletionToResponsesBody(doc, 1000, 1, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output len = %d, want 2:\n%+v", len(output), output)
	}
	first, _ := output[0].(map[string]any)
	summary, _ := first["summary"].([]any)
	sm, _ := summary[0].(map[string]any)
	if first["type"] != "reasoning" || sm["text"] != "内嵌思考" {
		t.Errorf("reasoning = %+v, want 内嵌思考", first)
	}
	second, _ := output[1].(map[string]any)
	content, _ := second["content"].([]any)
	cm, _ := content[0].(map[string]any)
	if cm["text"] != "干净正文" {
		t.Errorf("message text = %v, want 干净正文 (标签与思考应被剥离)", cm["text"])
	}
}

// TestChatStreamReasoningFieldSeparated 验证流式：reasoning_content delta 走
// reasoning_summary_text.delta，正文 content 走 output_text.delta，互不污染。
func TestChatStreamReasoningFieldSeparated(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	var sb strings.Builder
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"思考A"},"finish_reason":null}]}`))
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"思考B"},"finish_reason":null}]}`))
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"正文X"},"finish_reason":null}]}`))
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	sb.WriteString(tr.Flush())
	sse := sb.String()

	reasoningText := collectDeltaText(sse, "response.reasoning_summary_text.delta")
	if !strings.Contains(reasoningText, "思考A") || !strings.Contains(reasoningText, "思考B") {
		t.Errorf("reasoning deltas missing 思考A/B: %q", reasoningText)
	}
	joinedText := collectDeltaText(sse, "response.output_text.delta")
	if !strings.Contains(joinedText, "正文X") {
		t.Errorf("text delta missing 正文X: %q", joinedText)
	}
	if strings.Contains(joinedText, "思考") {
		t.Errorf("reasoning leaked into output_text: %q", joinedText)
	}
	// reasoning 项必须收尾。
	if len(collectFrameData(sse, "response.reasoning_summary_text.done")) == 0 {
		t.Errorf("missing reasoning_summary_text.done:\n%s", sse)
	}
}

// TestChatStreamInlineThinkCrossChunk 验证流式：正文内嵌 <think> 被切在多个 chunk 时，
// 状态机也能正确剥离——思考进 reasoning，标签不进正文。
func TestChatStreamInlineThinkCrossChunk(t *testing.T) {
	tr := newChatSSETransformer("m", 1, nil)
	var sb strings.Builder
	// "<think>" 被切成 "<th" + "ink>思考</think>正文"
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"<th"},"finish_reason":null}]}`))
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"ink>思考</think>正文"},"finish_reason":null}]}`))
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	sb.WriteString(tr.Flush())
	sse := sb.String()

	joinedReason := collectDeltaText(sse, "response.reasoning_summary_text.delta")
	if !strings.Contains(joinedReason, "思考") {
		t.Errorf("inline think not routed to reasoning: %q", joinedReason)
	}
	joinedText := collectDeltaText(sse, "response.output_text.delta")
	if !strings.Contains(joinedText, "正文") {
		t.Errorf("post-think text missing: %q", joinedText)
	}
	if strings.Contains(joinedText, "think") || strings.Contains(joinedText, "思考") {
		t.Errorf("think tag/content leaked into output_text: %q", joinedText)
	}
}

// TestChatStreamPlainTextNoThink 验证不含 <think> 的普通正文不被误判、无泄漏。
func TestChatStreamPlainTextNoThink(t *testing.T) {
	tr := newChatSSETransformer("m", 1, nil)
	var sb strings.Builder
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`))
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`))
	sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	sb.WriteString(tr.Flush())
	sse := sb.String()

	joinedText := collectDeltaText(sse, "response.output_text.delta")
	if joinedText != "Hello world" {
		t.Errorf("plain text = %q, want 'Hello world'", joinedText)
	}
	if len(collectFrameData(sse, "response.reasoning_summary_text.delta")) != 0 {
		t.Errorf("no reasoning expected for plain text:\n%s", sse)
	}
}

// chatMessagesForInput 跑请求方向转换并返回 messages 数组，便于断言。
func chatMessagesForInput(t *testing.T, input []any) []any {
	t.Helper()
	out, err := responsesToChatRequest(map[string]any{
		"model": "deepseek-v4-flash",
		"input": input,
	}, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	msgs, ok := out["messages"].([]any)
	if !ok {
		t.Fatalf("messages not array: %T", out["messages"])
	}
	return msgs
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not a map: %T", v)
	}
	return m
}

// TestChatCoalescesCommentaryWithToolCalls 验证「commentary 文本 assistant + 紧跟
// function_call」合并进同一条 Chat assistant 消息（对齐 cc-switch），避免纯文本
// assistant 单独成条被模型当成完整回合而停手。
func TestChatCoalescesCommentaryWithToolCalls(t *testing.T) {
	msgs := chatMessagesForInput(t, []any{
		map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "need update"}}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Part 1 done."}}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"a.go"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "contents"},
	})
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (合并后 assistant + tool)", len(msgs))
	}
	m0 := asMap(t, msgs[0])
	if m0["role"] != "assistant" || m0["content"] != "Part 1 done." {
		t.Errorf("messages[0] = %v, want assistant commentary", m0)
	}
	calls, ok := m0["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("messages[0].tool_calls = %v, want 1 合并进来的调用", m0["tool_calls"])
	}
	if asMap(t, calls[0])["id"] != "call_1" {
		t.Errorf("tool_call id = %v, want call_1", asMap(t, calls[0])["id"])
	}
	if m0["reasoning_content"] != "need update" {
		t.Errorf("reasoning_content = %v, want 'need update'", m0["reasoning_content"])
	}
	if asMap(t, msgs[1])["role"] != "tool" {
		t.Errorf("messages[1].role = %v, want tool", asMap(t, msgs[1])["role"])
	}
}

// TestChatKeepsUserBoundaryBeforeToolCall 验证 user 回合边界不会被合并：文本
// assistant / user / 工具调用应分成三条，工具调用另起 assistant。
func TestChatKeepsUserBoundaryBeforeToolCall(t *testing.T) {
	msgs := chatMessagesForInput(t, []any{
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Done for now."}}},
		map[string]any{"type": "message", "role": "user", "content": "Continue."},
		map[string]any{"type": "function_call", "call_id": "cn", "name": "a", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "cn", "output": "r"},
	})
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	m0 := asMap(t, msgs[0])
	if _, has := m0["tool_calls"]; has {
		t.Errorf("commentary 不应合并跨 user 的工具调用: %v", m0)
	}
	if asMap(t, msgs[1])["role"] != "user" {
		t.Errorf("messages[1].role = %v, want user", asMap(t, msgs[1])["role"])
	}
	m2 := asMap(t, msgs[2])
	if m2["role"] != "assistant" || m2["content"] != nil {
		t.Errorf("messages[2] = %v, want 独立 assistant tool-call 消息", m2)
	}
	if calls, _ := m2["tool_calls"].([]any); len(calls) != 1 || asMap(t, calls[0])["id"] != "cn" {
		t.Errorf("messages[2].tool_calls = %v, want cn", m2["tool_calls"])
	}
}

// TestChatDeduplicatesToolCallReasoning 验证并行 tool_calls 的重复思考段被去重合并，
// 不同段用 \n\n 拼接。
func TestChatDeduplicatesToolCallReasoning(t *testing.T) {
	call1 := map[string]any{"type": "function_call", "call_id": "c1", "name": "a", "arguments": `{}`, "reasoning_content": "need update"}
	call2 := map[string]any{"type": "function_call", "call_id": "c2", "name": "b", "arguments": `{}`, "reasoning_content": "second plan"}
	msgs := chatMessagesForInput(t, []any{
		map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "need update"}}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "P1"}}},
		call1,
		call2,
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "o1"},
		map[string]any{"type": "function_call_output", "call_id": "c2", "output": "o2"},
	})
	m0 := asMap(t, msgs[0])
	if m0["reasoning_content"] != "need update\n\nsecond plan" {
		t.Errorf("reasoning_content = %q, want 'need update\\n\\nsecond plan'（重复段去重）", m0["reasoning_content"])
	}
	if calls, _ := m0["tool_calls"].([]any); len(calls) != 2 {
		t.Errorf("tool_calls = %v, want 2 个并行调用合并到一条", m0["tool_calls"])
	}
}

// TestChatBackfillsToolCallReasoningPlaceholder 验证带 tool_calls 但没有任何思考的
// assistant 消息会被补上 reasoning_content 占位（thinking 模型强制要求）。
func TestChatBackfillsToolCallReasoningPlaceholder(t *testing.T) {
	msgs := chatMessagesForInput(t, []any{
		map[string]any{"type": "function_call", "call_id": "cb", "name": "a", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "cb", "output": "r"},
	})
	m0 := asMap(t, msgs[0])
	if m0["reasoning_content"] != "tool call" {
		t.Errorf("reasoning_content = %v, want 占位 'tool call'", m0["reasoning_content"])
	}
}

// TestChatStreamMultiToolCallDoneOrder 验证流式并行工具调用收尾时 output_item.done 帧
// 按 output_index 升序排列。此前 flushFinish 直接 range map，迭代顺序随机会导致 done
// 帧乱序，Codex 侧按顺序拼接会错配 call。
func TestChatStreamMultiToolCallDoneOrder(t *testing.T) {
	for trial := 0; trial < 30; trial++ {
		tr := newChatSSETransformer("m", 1, nil)
		var sb strings.Builder
		sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t0","function":{"name":"a","arguments":"{}"}}]},"finish_reason":null}]}`))
		sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"t1","function":{"name":"b","arguments":"{}"}}]},"finish_reason":null}]}`))
		sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"t2","function":{"name":"c","arguments":"{}"}}]},"finish_reason":null}]}`))
		sb.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
		sb.WriteString(tr.Flush())
		sse := sb.String()

		var doneIdx []string
		for _, f := range parseSSEFrames(sse) {
			if f.event != "response.output_item.done" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(f.data), &m); err != nil {
				continue
			}
			item, _ := m["item"].(map[string]any)
			if item == nil || item["type"] != "function_call" {
				continue
			}
			doneIdx = append(doneIdx, fmt.Sprintf("%v", m["output_index"]))
		}
		if strings.Join(doneIdx, ",") != "0,1,2" {
			t.Fatalf("trial %d: output_item.done 顺序 = %v, want [0 1 2]", trial, doneIdx)
		}
	}
}

// TestChatToolResultArrayMediaExtracted 验证 tool_result 的 output 为含媒体块的数组时，
// 文本留在 tool 消息、图片抽进紧跟的合成 user 消息（Chat tool 消息只能纯文本，直接
// stringifyAny 会把 []map 变成垃圾字面量丢图）。
func TestChatToolResultArrayMediaExtracted(t *testing.T) {
	msgs := chatMessagesForInput(t, []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "view_image", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": []any{
			map[string]any{"type": "output_text", "text": "here is the image"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		}},
	})
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (assistant + tool + 合成 user)", len(msgs))
	}
	tool := asMap(t, msgs[1])
	if tool["role"] != "tool" || tool["content"] != "here is the image" {
		t.Errorf("tool 消息应只留文本: %v", tool)
	}
	// content 不应含垃圾字面量
	if s, ok := tool["content"].(string); ok && strings.Contains(s, "map[") {
		t.Errorf("tool content 含垃圾字面量: %q", s)
	}
	usr := asMap(t, msgs[2])
	if usr["role"] != "user" {
		t.Fatalf("messages[2].role = %v, want user (媒体载体)", usr["role"])
	}
	parts, ok := usr["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("合成 user content = %v, want 1 个 image_url 块", usr["content"])
	}
	blk := asMap(t, parts[0])
	if blk["type"] != "image_url" {
		t.Errorf("media block type = %v, want image_url", blk["type"])
	}
	img := asMap(t, blk["image_url"])
	if img["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image url = %v, want 原 data URL", img["url"])
	}
}

// TestChatToolResultStringUnchanged 验证纯字符串 output 保持原纯文本 tool 消息，不追加 user。
func TestChatToolResultStringUnchanged(t *testing.T) {
	msgs := chatMessagesForInput(t, []any{
		map[string]any{"type": "function_call", "call_id": "c2", "name": "a", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "c2", "output": "plain result"},
	})
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	tool := asMap(t, msgs[1])
	if tool["role"] != "tool" || tool["content"] != "plain result" {
		t.Errorf("tool 消息 = %v, want 纯文本 'plain result'", tool)
	}
}

// TestChatToolResultMediaDegradesWhenUnsupported 验证不支持图片时媒体退化为提示文本
// 并入 tool 消息，不产生独立 user 媒体消息。
func TestChatToolResultMediaDegradesWhenUnsupported(t *testing.T) {
	out, err := responsesToChatRequest(map[string]any{
		"model": "deepseek-v4-flash",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "c3", "name": "a", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "c3", "output": []any{
				map[string]any{"type": "output_text", "text": "txt"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,BBBB"},
			}},
		},
	}, adapterOptions{SupportsImages: false})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (无独立媒体消息)", len(msgs))
	}
	tool := asMap(t, msgs[1])
	content, _ := tool["content"].(string)
	if !strings.Contains(content, "txt") || !strings.Contains(content, omittedImageText) {
		t.Errorf("退化文本应并入 tool 消息: %q", content)
	}
}
