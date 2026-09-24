package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsThinkingSignatureError(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"messages.1.content.0: Invalid 'signature' in 'thinking' block"}}`,
		`{"error":{"message":"Unable to submit request because Thought signature is not valid"}}`,
		`{"error":{"message":"a final assistant message must start with a thinking block"}}`,
		`{"error":{"message":"Expected thinking or redacted_thinking, but found tool_use"}}`,
		`{"error":{"message":"messages.signature: Field required"}}`,
		`{"error":{"message":"messages.signature: Extra inputs are not permitted"}}`,
		`{"error":{"message":"thinking blocks in the response cannot be modified"}}`,
		`{"error":{"message":"非法请求：thinking signature 不合法"}}`,
	} {
		if !isThinkingSignatureError(body) {
			t.Errorf("签名错误未识别: %s", body)
		}
	}
	for _, body := range []string{
		`{"error":{"message":"rate limit exceeded"}}`,
		`{"error":{"message":"invalid model"}}`,
		`{"error":{"message":"Expected thinking but found text"}}`,
		`{"error":{"message":"Request timeout"}}`,
		// 通用 400 且无 thinking/signature 上下文：不得误判为签名错误（M5），
		// 否则会把合法 thinking 块剥掉并白耗一次整流重试。
		`{"error":{"message":"invalid request: malformed JSON"}}`,
	} {
		if isThinkingSignatureError(body) {
			t.Errorf("非签名错误被误判: %s", body)
		}
	}
}

func TestIsThinkingBudgetError(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"thinking.budget_tokens: Input should be greater than or equal to 1024"}}`,
		`{"error":{"message":"thinking budget_tokens must be >= 1024"}}`,
	} {
		if !isThinkingBudgetError(body) {
			t.Errorf("budget 错误未识别: %s", body)
		}
	}
	for _, body := range []string{
		`{"error":{"message":"budget_tokens must be less than max_tokens"}}`,
		`{"error":{"message":"budget_tokens: value must be at least 1024"}}`,
		`{"error":{"message":"Request timeout"}}`,
	} {
		if isThinkingBudgetError(body) {
			t.Errorf("非 budget 错误被误判: %s", body)
		}
	}
}

func TestRectifyThinkingRequestSignature(t *testing.T) {
	body := []byte(`{
		"model":"claude-test",
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[{
			"role":"assistant",
			"content":[
				{"type":"thinking","thinking":"t","signature":"sig1"},
				{"type":"redacted_thinking","data":"r","signature":"sig2"},
				{"type":"tool_use","id":"toolu_1","name":"WebSearch","input":{},"signature":"sig3"},
				{"type":"text","text":"ok"}
			]
		},{
			"role":"user",
			"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]
		}]
	}`)
	rewritten, ok := rectifyThinkingRequest(body, false)
	if !ok {
		t.Fatal("thinking 签名整流未生效")
	}
	var doc map[string]any
	if err := json.Unmarshal(rewritten, &doc); err != nil {
		t.Fatalf("整流结果不是合法 JSON: %v", err)
	}
	messages := doc["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len = %d, want 2（thinking/redacted 已移除）", len(content))
	}
	for _, raw := range content {
		block := raw.(map[string]any)
		if _, has := block["signature"]; has {
			t.Error("整流后仍保留 signature 字段")
		}
		if bt := stringifyAny(block["type"]); bt == "thinking" || bt == "redacted_thinking" {
			t.Errorf("整流后仍保留 thinking 块: %s", bt)
		}
	}
	if _, has := doc["thinking"]; has {
		t.Error("最后 assistant 不以 thinking 开头，顶层 thinking 应被移除")
	}
}

func TestRectifyThinkingRequestBudget(t *testing.T) {
	body := []byte(`{
		"model":"claude-test",
		"thinking":{"type":"enabled","budget_tokens":512},
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}]
	}`)
	rewritten, ok := rectifyThinkingRequest(body, true)
	if !ok {
		t.Fatal("budget 整流未生效")
	}
	var doc map[string]any
	if err := json.Unmarshal(rewritten, &doc); err != nil {
		t.Fatalf("整流结果不是合法 JSON: %v", err)
	}
	thinking := doc["thinking"].(map[string]any)
	if stringifyAny(thinking["type"]) != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", stringifyAny(thinking["type"]))
	}
	if thinking["budget_tokens"].(float64) != maxThinkingBudget {
		t.Errorf("budget_tokens = %v, want %d", thinking["budget_tokens"], maxThinkingBudget)
	}
	if doc["max_tokens"].(float64) != maxThinkingMaxTokens {
		t.Errorf("max_tokens = %v, want %d", doc["max_tokens"], maxThinkingMaxTokens)
	}
}

func TestRectifyThinkingRequestNoChange(t *testing.T) {
	body := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`)
	if _, ok := rectifyThinkingRequest(body, false); ok {
		t.Error("无 thinking 问题时不应改写")
	}
	if _, ok := rectifyThinkingRequest(body, true); !ok {
		t.Error("budget 整流应在缺 thinking 字段时创建并改写")
	}
}

func TestRectifyThinkingRequestPreservesThinkingPrefix(t *testing.T) {
	body := []byte(`{
		"model":"claude-test",
		"thinking":{"type":"enabled"},
		"messages":[{
			"role":"assistant",
			"content":[
				{"type":"thinking","thinking":"t"},
				{"type":"tool_use","id":"toolu_1","name":"Test","input":{}}
			]
		}]
	}`)
	rewritten, ok := rectifyThinkingRequest(body, false)
	if !ok {
		t.Fatal("整流应移除 thinking 块")
	}
	if strings.Contains(string(rewritten), `"thinking"`) {
		t.Errorf("顶层 thinking 或块未被移除: %s", string(rewritten))
	}
}
