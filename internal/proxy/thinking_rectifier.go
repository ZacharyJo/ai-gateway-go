package proxy

import (
	"encoding/json"
	"strings"
)

// thinking 错误整流（对齐 cc-switch thinking_rectifier + thinking_budget_rectifier）。
//
// 适配器出站 body 是 Messages/Chat 格式，客户端历史可能携带 thinking 签名块；
// 上游对这类签名/budget 错误直接 400 会中断整个对话。这里复用 image_fallback 的
// "上游拒绝 -> 改写请求体 -> 单次重试" 模式，让对话继续而不是把权威错误交给用户。

const (
	maxThinkingBudget     = 32000
	maxThinkingMaxTokens  = 64000
	minMaxTokensForBudget = maxThinkingBudget + 1
)

// isThinkingSignatureError 判断上游错误体是否命中 Anthropic thinking 签名约束。
// 规则来自 cc-switch 已实测/测试覆盖的错误形态，保持最小匹配面避免误伤。
func isThinkingSignatureError(body string) bool {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "invalid") && strings.Contains(lower, "signature") &&
		strings.Contains(lower, "thinking") && strings.Contains(lower, "block") {
		return true
	}
	if strings.Contains(lower, "thought signature") &&
		(strings.Contains(lower, "not valid") || strings.Contains(lower, "invalid")) {
		return true
	}
	if strings.Contains(lower, "must start with a thinking block") {
		return true
	}
	if strings.Contains(lower, "expected") &&
		(strings.Contains(lower, "thinking") || strings.Contains(lower, "redacted_thinking")) &&
		strings.Contains(lower, "found") && strings.Contains(lower, "tool_use") {
		return true
	}
	if strings.Contains(lower, "signature") && strings.Contains(lower, "field required") {
		return true
	}
	if strings.Contains(lower, "signature") && strings.Contains(lower, "extra inputs are not permitted") {
		return true
	}
	if (strings.Contains(lower, "thinking") || strings.Contains(lower, "redacted_thinking")) &&
		strings.Contains(lower, "cannot be modified") {
		return true
	}
	// 兜底通用措辞（invalid/illegal/非法 request）只在错误体确实提到 thinking/signature 时才算，
	// 否则任意无关 400（如坏参数、工具 schema 错误）都会被误判成签名错误，触发把合法 thinking
	// 块剥掉的整流重试并白耗一次重试机会。
	hasThinkingContext := strings.Contains(lower, "signature") ||
		strings.Contains(lower, "thinking") ||
		strings.Contains(lower, "redacted_thinking")
	if !hasThinkingContext {
		return false
	}
	return strings.Contains(lower, "非法请求") ||
		strings.Contains(lower, "illegal request") ||
		strings.Contains(lower, "invalid request")
}

// isThinkingBudgetError 判断上游错误体是否命中 thinking budget 约束。
// 与 cc-switch 一致：必须同时出现 budget_tokens + thinking + 1024 下限约束。
func isThinkingBudgetError(body string) bool {
	lower := strings.ToLower(body)
	hasBudget := strings.Contains(lower, "budget_tokens") || strings.Contains(lower, "budget tokens")
	hasThinking := strings.Contains(lower, "thinking")
	has1024 := strings.Contains(lower, "greater than or equal to 1024") ||
		strings.Contains(lower, ">= 1024") ||
		(strings.Contains(lower, "1024") && strings.Contains(lower, "input should be"))
	return hasBudget && hasThinking && has1024
}

// thinkingRectifyResult 描述一次整流改写的结果（便于日志与测试断言）。
type thinkingRectifyResult struct {
	applied                 bool
	removedThinkingBlocks   int
	removedRedactedBlocks   int
	removedSignatureFields  int
	removedTopLevelThinking bool
	budgetSetThinking       bool
	budgetSetMaxTokens      bool
}

// rectifyThinkingRequest 对出站 Messages/Chat body 做最小侵入整流。
// 签名错误场景移除 thinking/redacted_thinking 块与遗留 signature 字段；
// budget 错误场景把 thinking.budget_tokens 提到 32000、max_tokens 提到 64000。
func rectifyThinkingRequest(body []byte, budget bool) ([]byte, bool) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	result := thinkingRectifyResult{}
	// 剥离 thinking 块/签名只在**签名错误**场景做；budget 错误只调 budget——
	// 剥离历史思考块会破坏多轮上下文且让签名链全部失效（超出 docstring 声明的行为）。
	if !budget {
		if messages, ok := doc["messages"].([]any); ok {
			for _, rawMsg := range messages {
				msg, ok := rawMsg.(map[string]any)
				if !ok {
					continue
				}
				content, ok := msg["content"].([]any)
				if !ok {
					continue
				}
				var newContent []any
				modified := false
				for _, rawBlock := range content {
					block, ok := rawBlock.(map[string]any)
					if !ok {
						newContent = append(newContent, rawBlock)
						continue
					}
					switch strings.ToLower(stringifyAny(block["type"])) {
					case "thinking":
						result.removedThinkingBlocks++
						modified = true
						continue
					case "redacted_thinking":
						result.removedRedactedBlocks++
						modified = true
						continue
					}
					if _, has := block["signature"]; has {
						delete(block, "signature")
						result.removedSignatureFields++
						modified = true
					}
					newContent = append(newContent, block)
				}
				if modified {
					result.applied = true
					msg["content"] = newContent
				}
			}
		}
		if shouldRemoveTopLevelThinking(doc) {
			delete(doc, "thinking")
			result.applied = true
			result.removedTopLevelThinking = true
		}
	}
	if budget {
		if applyThinkingBudget(doc, &result) {
			result.applied = true
		}
	}
	if !result.applied {
		return nil, false
	}
	rewritten, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return rewritten, true
}

// shouldRemoveTopLevelThinking 判断工具调用链中最后一条 assistant 不以
// thinking 块开头时，是否应移除顶层 thinking 开关（对齐 cc-switch）。
func shouldRemoveTopLevelThinking(doc map[string]any) bool {
	thinking, ok := doc["thinking"].(map[string]any)
	if !ok || stringifyAny(thinking["type"]) != "enabled" {
		return false
	}
	messages, _ := doc["messages"].([]any)
	var lastAssistant map[string]any
	for i := len(messages) - 1; i >= 0; i-- {
		if m, ok := messages[i].(map[string]any); ok && stringifyAny(m["role"]) == "assistant" {
			lastAssistant = m
			break
		}
	}
	if lastAssistant == nil {
		return false
	}
	content, _ := lastAssistant["content"].([]any)
	if len(content) == 0 {
		return false
	}
	first, _ := content[0].(map[string]any)
	if first != nil {
		switch strings.ToLower(stringifyAny(first["type"])) {
		case "thinking", "redacted_thinking":
			return false
		}
	}
	for _, rawBlock := range content {
		if block, ok := rawBlock.(map[string]any); ok && stringifyAny(block["type"]) == "tool_use" {
			return true
		}
	}
	return false
}

// applyThinkingBudget 把 budget_tokens 提到 32000；max_tokens 低于 32001 时提到 64000。
func applyThinkingBudget(doc map[string]any, result *thinkingRectifyResult) bool {
	thinking, ok := doc["thinking"].(map[string]any)
	if !ok {
		thinking = map[string]any{}
		doc["thinking"] = thinking
	}
	if stringifyAny(thinking["type"]) == "adaptive" {
		return false
	}
	if stringifyAny(thinking["type"]) != "enabled" {
		thinking["type"] = "enabled"
		result.budgetSetThinking = true
	}
	if n, ok := thinking["budget_tokens"].(float64); !ok || n != maxThinkingBudget {
		thinking["budget_tokens"] = float64(maxThinkingBudget)
		result.budgetSetThinking = true
	}
	maxTokens, _ := doc["max_tokens"].(float64)
	if maxTokens == 0 || maxTokens < minMaxTokensForBudget {
		doc["max_tokens"] = float64(maxThinkingMaxTokens)
		result.budgetSetMaxTokens = true
	}
	return result.budgetSetThinking || result.budgetSetMaxTokens
}
