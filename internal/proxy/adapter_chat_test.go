package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// extractFrameData 从 Responses SSE 输出里取出指定 event 的 data 载荷。
func extractFrameData(t *testing.T, sse, event string) []byte {
	t.Helper()
	for _, f := range parseSSEFrames(sse) {
		if f.event == event {
			return []byte(f.data)
		}
	}
	t.Fatalf("event %q not found in:\n%s", event, sse)
	return nil
}

// TestChatSSETransformerUsageAfterFinish 验证 OpenAI 标准顺序
// （finish chunk 之后接 usage-only chunk）时 response.completed 能带上 usage。
// 回归：之前 usage chunk 排在 finish 之后，accumulate 后 completed 已发出，usage 永远丢。
func TestChatSSETransformerUsageAfterFinish(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	// 1. 内容增量
	if out := tr.Push(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`); out == "" {
		t.Fatal("content chunk produced no output")
	}
	// 2. finish chunk：只收尾各 item，不得提前发 response.completed
	out := tr.Push(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if strings.Contains(out, "response.completed") {
		t.Fatalf("response.completed emitted before usage chunk:\n%s", out)
	}
	// 3. usage-only chunk：补发 response.completed 并带上刚累计的 usage
	out = tr.Push(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	var completed struct {
		Response struct {
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				TotalTokens  int64 `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(extractFrameData(t, out, "response.completed"), &completed); err != nil {
		t.Fatalf("unmarshal completed: %v\n%s", err, out)
	}
	if completed.Response.Usage.InputTokens != 10 || completed.Response.Usage.OutputTokens != 5 ||
		completed.Response.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v, want 10/5/15", completed.Response.Usage)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("expected trailing [DONE], got:\n%s", out)
	}
	// 4. 之后再 Flush 不应重复产出
	if out := tr.Flush(); out != "" {
		t.Errorf("Flush after done should be empty, got:\n%s", out)
	}
}

// TestChatSSETransformerUsageSameChunkAsFinish 验证 usage 与 finish_reason 同帧时
// 也能带上（部分上游把 usage 合并进最后一个 chunk）。
func TestChatSSETransformerUsageSameChunkAsFinish(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	// finish 已到，completed 被挂起；EOF 后 Flush 补发
	out := tr.Flush()
	if !strings.Contains(out, `"input_tokens":3`) || !strings.Contains(out, `"output_tokens":4`) {
		t.Fatalf("expected usage in completed:\n%s", out)
	}
}

// TestChatSSETransformerNoUsage 验证上游没给 usage 时不臆造 0 值字段，completed 仍正常补发。
func TestChatSSETransformerNoUsage(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`)
	tr.Push(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	out := tr.Flush()
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("completed missing on clean end:\n%s", out)
	}
	if strings.Contains(out, `"usage"`) {
		t.Errorf("usage fabricated when upstream sent none:\n%s", out)
	}
}

// TestChatSSETransformerFlushEmptyStream 验证空流（无任何内容）直接 Flush 时
// 应按失败收尾，而不是谎报 completed（对齐 cc-switch stream_truncated）。
func TestChatSSETransformerFlushEmptyStream(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Flush()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_truncated") {
		t.Fatalf("empty stream should fail with stream_truncated:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("empty stream must not be completed:\n%s", out)
	}
}

func TestChatSSETransformerNoOrphanDoneForIncompleteTool(t *testing.T) {
	// 只有 index/id、name 从未到达的 tool call：Push 只发 created，不产生孤儿 .done。
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1"}]},"finish_reason":"stop"}]}`) + tr.Flush()
	if strings.Contains(out, "function_call_arguments.done") ||
		strings.Contains(out, "custom_tool_call_input.done") ||
		strings.Contains(out, "output_item.done") {
		t.Errorf("orphan .done emitted for tool call that never started:\n%s", out)
	}
	// 对照：name 到达的 tool call 在 Push 返回值里正常收尾（.done 帧）。
	tr2 := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out2 := tr2.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":"stop"}]}`) + tr2.Flush()
	if !strings.Contains(out2, "function_call_arguments.done") {
		t.Errorf("normal tool call should get .done:\n%s", out2)
	}
}

func TestChatSSETransformerDroppedToolFailsRound(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	// 上游声明工具调用但 name 缺失：收尾时不得谎报 completed，应 failed。
	out := tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1"}]},"finish_reason":"stop"}]}`) + tr.Flush()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "upstream_tool_call_dropped") {
		t.Fatalf("dropped tool round should fail with upstream_tool_call_dropped:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("dropped tool round must not be completed:\n%s", out)
	}
}

func TestChatSSETransformerDroppedToolKeepsOtherOutput(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	// 既有正文又有缺 name 的工具调用：cc-switch 仍判失败，因为模型本应继续工具链，
	// 却只剩一个空工具调用；不能把这种“答一句就停”伪装成 completed。
	out := tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"Done"},"finish_reason":null}]}`)
	out += tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"bad"}]},"finish_reason":"stop"}]}`)
	out += tr.Flush()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "upstream_tool_call_dropped") {
		t.Fatalf("round with text + dropped tool should fail:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("round with dropped tool must not complete:\n%s", out)
	}
}

func TestChatSSETransformerWhitespaceToolNameFailsRound(t *testing.T) {
	// name 是纯空白：与缺失 name 同口径（TrimSpace 后为空），既不算已启动、
	// 也不得下发给客户端，终态仍是 failed。
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"   ","arguments":"{}"}}]},"finish_reason":"stop"}]}`) + tr.Flush()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "upstream_tool_call_dropped") {
		t.Fatalf("whitespace tool name should fail:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("whitespace tool name must not complete:\n%s", out)
	}
	if strings.Contains(out, "function_call_arguments.done") || strings.Contains(out, `"name":"   "`) {
		t.Errorf("whitespace-named tool item leaked:\n%s", out)
	}
}

func TestChatSSETransformerDroppedToolWithUsableCallCompletes(t *testing.T) {
	// 同回合既有合法调用又有被丢弃的调用：只要下发过可执行调用（hasEmittedToolCall
	// 为真），就不该判失败——不能因为夹了一个空 name 就把整轮抹成 failed。
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ok","function":{"name":"bash","arguments":"{}"}},{"index":1,"id":"call_bad"}]},"finish_reason":"stop"}]}`)
	out += tr.Flush()
	if strings.Contains(out, "response.failed") {
		t.Fatalf("round with a usable tool call must not fail:\n%s", out)
	}
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("expected response.completed:\n%s", out)
	}
	if !strings.Contains(out, `"call_id":"call_ok"`) {
		t.Errorf("usable tool call not emitted:\n%s", out)
	}
}

func TestChatSSETransformerStreamErrorFails(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Push(`data: {"error":{"message":"quota exceeded","type":"rate_limit_exceeded"}}`) + tr.Flush()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "quota exceeded") {
		t.Fatalf("stream error should fail with payload:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("stream error must not complete:\n%s", out)
	}
}

// TestChatSSETransformerInlineThinkVariants 验证 think 标签变体在流式路径下也能
// 归入 reasoning、正文保持干净——含标签被切在多个 chunk 的情形。
// 回归：此前只认字面量 <think>/</think>，<thinking> 与带空白的标签整块穿透进 output_text，
// codex 会把它们当普通文本渲染出来。
func TestChatSSETransformerInlineThinkVariants(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		wantR  string
		wantT  string
	}{
		{"thinking split across chunks", []string{"<thi", "nking>思考", "中</thin", "king>正文"}, "思考中", "正文"},
		{"thinking single chunk", []string{"<thinking>思考</thinking>正文"}, "思考", "正文"},
		{"open tag with space", []string{"<think >思考</think >正文"}, "思考", "正文"},
		{"bare think unchanged", []string{"<think>思考</think>正文"}, "思考", "正文"},
		// "<" 后跟空白永远长不成标签，必须当正文立即下发，不能攒到 flush 才吐。
		{"lt space is plain text", []string{"< 5", ">3"}, "", "< 5>3"},
		// 单独的 "<" 仍要缓冲：下一 chunk 才到齐 "<thinking>"，思考仍归 reasoning。
		{"bare lt then thinking split", []string{"<", "thinking>思考", "</thinking>正文"}, "思考", "正文"},
		{"plain text unaffected", []string{"hello"}, "", "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
			var sse strings.Builder
			for _, d := range c.chunks {
				payload, err := json.Marshal(map[string]any{
					"id": "c",
					"choices": []any{map[string]any{
						"index": 0, "delta": map[string]any{"content": d}, "finish_reason": nil,
					}},
				})
				if err != nil {
					t.Fatalf("marshal chunk: %v", err)
				}
				// 内容在 Push 时就逐帧下发，只收 Flush 会漏掉全部 delta。
				sse.WriteString(tr.Push(string(payload)))
			}
			sse.WriteString(tr.Flush())
			out := sse.String()
			if got := collectDeltaText(out, "response.reasoning_summary_text.delta"); got != c.wantR {
				t.Errorf("reasoning = %q, want %q\n%s", got, c.wantR, out)
			}
			if got := collectDeltaText(out, "response.output_text.delta"); got != c.wantT {
				t.Errorf("text = %q, want %q\n%s", got, c.wantT, out)
			}
		})
	}
}

// TestChatSSETransformerUnclosedThink 验证流式下 <thinking> 未闭合（思考被 max_tokens 截断，
// 或模型漏打闭合标签）时，缓冲内容必须作为正文兜底下发，不能整段吞进 reasoning——
// 否则这一轮只有 reasoning 没有 message，codex 会把"还没执行完"的轮次提前收尾。
// 回归：v0.2.2 起 <thinking> 被识别为思考块，未闭合块在 flushInlineThink 被整个转成
// reasoning，output_text 为空。
func TestChatSSETransformerUnclosedThink(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	var sse strings.Builder
	for _, d := range []string{
		"<thinking>正在思考这个问题",
		"比较复杂，先给结论：答案是42",
	} {
		sse.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"` + d + `"},"finish_reason":null}]}`))
	}
	sse.WriteString(tr.Push(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	out := sse.String() + tr.Flush()
	if got := collectDeltaText(out, "response.reasoning_summary_text.delta"); got != "" {
		t.Errorf("unclosed think must NOT become reasoning, got %q", got)
	}
	want := "正在思考这个问题比较复杂，先给结论：答案是42"
	if got := collectDeltaText(out, "response.output_text.delta"); got != want {
		t.Errorf("text = %q, want %q\n%s", got, want, out)
	}
	if !strings.Contains(out, "response.completed") {
		t.Errorf("round must complete with a message, got:\n%s", out)
	}
}

// TestApplyChatToolChoiceEnums 验证 Chat 适配的 tool_choice 与 Messages 版对齐：
// map 形式的 none/auto/required 枚举都被处理（此前只认命名工具）。
func TestApplyChatToolChoiceEnums(t *testing.T) {
	tools := []any{map[string]any{"name": "read_file"}}
	newOut := func() map[string]any { return map[string]any{"tools": tools} }

	// auto → Chat 的 "auto"
	out := newOut()
	applyChatToolChoice(out, map[string]any{"type": "auto"})
	if out["tool_choice"] != "auto" {
		t.Errorf(`{"type":"auto"} = %v, want "auto"`, out["tool_choice"])
	}
	// required / any → Chat 的 "required"
	for _, v := range []string{"required", "any"} {
		out = newOut()
		applyChatToolChoice(out, map[string]any{"type": v})
		if out["tool_choice"] != "required" {
			t.Errorf(`{"type":%q} = %v, want "required"`, v, out["tool_choice"])
		}
	}
	// none → 去掉 tools（等价禁止调用）
	out = newOut()
	applyChatToolChoice(out, map[string]any{"type": "none"})
	if _, has := out["tools"]; has {
		t.Errorf(`{"type":"none"} left tools in place: %v`, out)
	}
	// 命名工具 → Chat 的 function 对象
	out = newOut()
	applyChatToolChoice(out, map[string]any{"type": "function", "name": "read_file"})
	tc, _ := out["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "function" {
		t.Errorf("named tool = %v, want Chat function object", out["tool_choice"])
	}
}

func TestChatSSETransformerNullErrorNotFatal(t *testing.T) {
	// 网关常在每个 chunk 回显 "error":null：不能判为流失败。
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"error":null}`) + tr.Flush()
	if strings.Contains(out, "response.failed") {
		t.Errorf(`"error":null 不应判失败:\n%s`, out)
	}
	if !strings.Contains(out, "response.completed") {
		t.Errorf("正常流应收尾 completed:\n%s", out)
	}
}

func TestChatSSETransformerWhitespaceThenRealName(t *testing.T) {
	// 首帧 name 为纯空白、后续帧才给真实 name：应更新为真实 name，不得永久判丢弃。
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"   "}}]}}]}`)
	out += tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"bash","arguments":"{}"}}]},"finish_reason":"stop"}]}`)
	out += tr.Flush()
	if strings.Contains(out, "upstream_tool_call_dropped") {
		t.Errorf("空白占位后到达真实 name 不应判丢弃:\n%s", out)
	}
	if !strings.Contains(out, "function_call_arguments.done") {
		t.Errorf("合法工具调用应正常收尾:\n%s", out)
	}
}

func TestChatSSETransformerTruncatedDroppedToolClassified(t *testing.T) {
	// 无 name 的工具调用 + 无 finish_reason 截断：应报 upstream_tool_call_dropped
	// （与 Messages 路径同口径），而非 stream_truncated。
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"bad"}]}}]}`) + tr.Flush()
	if !strings.Contains(out, "upstream_tool_call_dropped") {
		t.Errorf("截断+丢弃工具应报 upstream_tool_call_dropped:\n%s", out)
	}
	if strings.Contains(out, "stream_truncated") {
		t.Errorf("不应报 stream_truncated:\n%s", out)
	}
}

func TestChatSSETransformerTerminalFailed(t *testing.T) {
	// 丢弃工具回合应报告 TerminalFailed=true，供监控记非 200（否则客户端看到 failed、日志记 200）。
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	tr.Push(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"bad"}]},"finish_reason":"stop"}]}`)
	tr.Flush()
	if !tr.TerminalFailed() {
		t.Error("丢弃工具回合应报告 TerminalFailed=true")
	}
}
