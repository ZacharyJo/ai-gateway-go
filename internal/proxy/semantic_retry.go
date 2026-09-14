package proxy

import (
	"encoding/json"
	"strings"
)

// reasoning-only 空转重试。
// 现象：模型只吐 reasoning、没吐任何可执行输出（message/function_call）就 response.completed，
// 而上一条输入恰好是等待中的工具输出 —— 这一轮等于空转，客户端看到空回复。
// 处理：符合资格的请求首次响应全缓冲，判定为空转就丢弃并原样重打一次（第二次走正常流式）。
// 代价是命中资格的请求首字延迟 = 整个响应时长，所以资格判定收得很窄。

// actionableOutputTypes 是"可执行输出"类型：出现任一即说明这轮不是空转。
var actionableOutputTypes = map[string]bool{
	"message": true, "function_call": true, "custom_tool_call": true,
}

// pendingToolOutputTypes 是"等待中的工具输出"类型：作为 input 最后一项时说明模型该继续干活。
var pendingToolOutputTypes = map[string]bool{
	"function_call_output": true, "custom_tool_call_output": true,
}

// modelMatchesReasoningRetry 判断模型是否在重试名单内：全等或以 "<pattern>-" 开头
// （所以 gpt-5.6 命中 gpt-5.6-terra）。
func modelMatchesReasoningRetry(model string, patterns []string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if m == p || strings.HasPrefix(m, p+"-") {
			return true
		}
	}
	return false
}

// hasPendingToolOutput 判断 input 最后一项是否为等待中的工具输出。
func hasPendingToolOutput(doc map[string]any) bool {
	input, ok := doc["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}
	last, ok := input[len(input)-1].(map[string]any)
	if !ok {
		return false
	}
	return pendingToolOutputTypes[stringifyAny(last["type"])]
}

// responsesSSESummary 是对一段完整 Responses SSE 的语义判定。
type responsesSSESummary struct {
	completed  bool // 见到 response.completed
	terminal   bool // 见到 completed/failed/incomplete 任一（流正常收尾）
	actionable bool // 见到可执行输出项
	hasEvents  bool // 见到至少一个 response.* 事件（区分真空转与网络截断）
}

// summarizeResponsesSSE 解析缓冲的 Responses SSE，判定是否收尾、是否有可执行输出。
func summarizeResponsesSSE(text string) responsesSSESummary {
	var s responsesSSESummary
	for _, frame := range parseSSEFrames(text) {
		data := strings.TrimSpace(frame.data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			continue
		}
		evtType := stringifyAny(parsed["type"])
		switch evtType {
		case "response.completed":
			s.completed = true
			s.terminal = true
		case "response.failed", "response.incomplete":
			s.terminal = true
		}
		// 任意 response.* 事件都标记 hasEvents，区分真空转与网络截断
		if strings.HasPrefix(evtType, "response.") {
			s.hasEvents = true
		}
		// 事件自带的 output 项（output_item.added/done）
		if item, ok := parsed["item"].(map[string]any); ok {
			if actionableOutputTypes[stringifyAny(item["type"])] {
				s.actionable = true
			}
		}
		// response.completed 里的完整 output 数组
		if response, ok := parsed["response"].(map[string]any); ok {
			if output, ok := response["output"].([]any); ok {
				for _, raw := range output {
					if item, ok := raw.(map[string]any); ok && actionableOutputTypes[stringifyAny(item["type"])] {
						s.actionable = true
					}
				}
			}
		}
	}
	return s
}

// reasoningRetryPlan 是一次请求的空转重试资格。
type reasoningRetryPlan struct {
	eligible bool
	model    string
}

// planReasoningRetry 判断请求是否具备空转重试资格：开关开启 + 上限 > 0 + 模型命中名单
// + input 最后一项是等待中的工具输出。只对 POST /responses 有意义。
func planReasoningRetry(cfg *Config, path string, body []byte) reasoningRetryPlan {
	if !cfg.ReasoningOnlyRetryEnabled || cfg.ReasoningOnlyRetryMax <= 0 || path != "/responses" {
		return reasoningRetryPlan{}
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return reasoningRetryPlan{}
	}
	model := stringifyAny(doc["model"])
	if !modelMatchesReasoningRetry(model, cfg.ReasoningOnlyRetryModels) {
		return reasoningRetryPlan{}
	}
	if !hasPendingToolOutput(doc) {
		return reasoningRetryPlan{}
	}
	return reasoningRetryPlan{eligible: true, model: model}
}

// shouldRetryReasoningOnly 判断缓冲到的响应是否为空转（需要丢弃重打）。
// completed 但无可执行输出，或流没正常收尾。
//
// 注意：!s.terminal 会把网络截断（RST/空闲超时后有部分数据）也误判为空转。
// 调用方已通过 len(buffered)==0 过滤掉了完全空的截断；此处进一步要求
// 流必须含至少一个 response.* 事件（s.hasEvents），无事件说明是初始截断而非空转。
func shouldRetryReasoningOnly(s responsesSSESummary) bool {
	if s.completed && !s.actionable {
		return true
	}
	// !s.terminal 且有事件：流开始了但未正常收尾，才视为空转（而非网络截断）
	return !s.terminal && s.hasEvents
}
