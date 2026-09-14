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

// TestChatSSETransformerFlushEmptyStream 验证空流（无任何内容）直接 Flush 也能收尾。
func TestChatSSETransformerFlushEmptyStream(t *testing.T) {
	tr := newChatSSETransformer("deepseek-v4-flash", 1, nil)
	out := tr.Flush()
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("completed missing on empty stream:\n%s", out)
	}
}
