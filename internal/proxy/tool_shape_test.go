package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnwrapCustomToolInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single key input", `{"input":"ls -la"}`, "ls -la"},
		{"script key", `{"script":"#!/bin/sh\n"}`, "#!/bin/sh\n"},
		{"plain text not json", "run the linter", "run the linter"},
		{"json string", `"just a string"`, "just a string"},
		{"nested input", `{"input":{"input":"x"}}`, "x"},
		{"non-envelope json", `{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{"empty", "", ""},
		{"array json", `["a"]`, `["a"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unwrapCustomToolInput(c.in); got != c.want {
				t.Fatalf("unwrapCustomToolInput(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestRepairResponsesBodyToolShapes(t *testing.T) {
	custom := map[string]bool{"exec": true, "apply_patch": true}
	input := map[string]any{
		"id": "resp_1",
		"output": []any{
			map[string]any{
				"type": "function_call", "name": "exec", "call_id": "fc_call_1",
				"arguments": `{"input":"git diff"}`,
			},
			map[string]any{
				"type": "function_call", "name": "regular", "call_id": "fc_call_2",
				"arguments": `{"q":"x"}`,
			},
			map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "ok"}},
			},
		},
	}
	got, repaired := repairResponsesBodyToolShapes(input, custom)
	if len(repaired) != 1 || repaired[0].Tool != "exec" {
		t.Fatalf("expected 1 repaired exec, got %+v", repaired)
	}
	out := got["output"].([]any)
	first := out[0].(map[string]any)
	if first["type"] != "custom_tool_call" {
		t.Fatalf("first item type = %v, want custom_tool_call", first["type"])
	}
	if first["input"] != "git diff" {
		t.Fatalf("first item input = %v, want git diff", first["input"])
	}
	if _, has := first["arguments"]; has {
		t.Fatalf("custom_tool_call should not carry arguments")
	}
	// 非 custom 的 function_call 原样保留
	second := out[1].(map[string]any)
	if second["type"] != "function_call" {
		t.Fatalf("regular tool should stay function_call, got %v", second["type"])
	}

	// 未声明 custom 工具时不动
	if _, r := repairResponsesBodyToolShapes(input, nil); r != nil {
		t.Fatalf("nil customTools should not repair")
	}
}

func TestToolShapeRepairerSSE(t *testing.T) {
	custom := map[string]bool{"exec": true}
	repairer := newToolShapeRepairer(custom)
	// 模拟上游降级：added（function_call）→ arguments.delta → arguments.done → output_item.done
	events := []string{
		sseFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{
				"type": "function_call", "name": "exec", "id": "fc_call_1", "arguments": "",
			},
		}),
		sseFrame("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "fc_call_1", "delta": `{"input":"ru`,
		}),
		sseFrame("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "fc_call_1", "delta": `n tests"}`,
		}),
		sseFrame("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "fc_call_1",
			"arguments": `{"input":"run tests"}`,
		}),
		sseFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{
				"type": "function_call", "name": "exec", "id": "fc_call_1", "arguments": `{"input":"run tests"}`,
			},
		}),
		sseFrame("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"output": []any{
					map[string]any{"type": "function_call", "name": "exec", "id": "fc_call_1", "arguments": `{"input":"run tests"}`},
				},
			},
		}),
	}

	// 分块喂入，模拟跨 chunk
	var out strings.Builder
	chunks := []string{events[0][:10], events[0][10:] + events[1] + events[2][:5], events[2][5:] + events[3] + events[4] + events[5]}
	for _, c := range chunks {
		out.WriteString(repairer.Push(c))
	}
	out.WriteString(repairer.Flush())

	text := out.String()
	// 不能残留原始 function_call（除 completed 里应被修掉）
	if strings.Contains(text, `"type":"function_call"`) && strings.Contains(text, `"name":"exec"`) {
		t.Fatalf("output still contains degraded function_call:\n%s", text)
	}
	// 应出现 custom_tool_call 与输入事件
	for _, want := range []string{"custom_tool_call", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", `"input":"run tests"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
	// 断言所有事件都能被重新解析成合法 SSE
	frames := parseSSEFrames(text)
	if len(frames) == 0 {
		t.Fatalf("no frames emitted")
	}
	for _, f := range frames {
		if f.data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(f.data), &m); err != nil {
			t.Fatalf("frame %s has invalid json: %v", f.event, err)
		}
	}
}
