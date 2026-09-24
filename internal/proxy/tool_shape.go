package proxy

import (
	"encoding/json"
	"strings"
)

// 工具形状修理（对应 ai-gateway 的 tool_shape）：
// 修复把客户端声明的 `custom`（freeform）工具降级成普通 function_call 的上游响应。
//
// 背景：Codex 把 exec / apply_patch 声明为 { type: "custom" } 工具，只有模型回
// custom_tool_call 时才能派发。部分上游 OneAPI 通道在进站时把 custom 工具包成
// 单参 function_call，回程忘了解包，客户端收到 function_call + {"input":"..."}
// 后只记录不产出，回合卡死、重试耗尽。
//
// 修法刻意保守：只碰客户端声明为 custom 的工具名，且 SSE 与非流式两条路径都覆盖。
// 与适配路径（adapter.go toolUseToCustomToolCall）的区别：那边是 Messages→Responses
// 转换时还原，这边是 Responses 透传（GPT 系）时修上游通道的降级。

// freeformInputKeys 是单 key JSON 信封的可能键：解包时按序找第一个字符串值。
var freeformInputKeys = []string{"input", "script", "text", "content", "code"}

// unwrapCustomToolInput 从被包装的 function-call 参数字符串解出 freeform 纯文本。
// 不是单 key JSON 信封时原样返回，保证真正的自由文本 body 不被误改。
func unwrapCustomToolInput(argumentsText string) string {
	text := strings.TrimSpace(argumentsText)
	if text == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return text
	}
	if s, ok := parsed.(string); ok {
		return s
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return text
	}
	for _, key := range freeformInputKeys {
		value, has := m[key]
		if !has {
			continue
		}
		if s, ok := value.(string); ok {
			return s
		}
		if nested, ok := value.(map[string]any); ok {
			if s, ok := nested["input"].(string); ok {
				return s
			}
		}
	}
	return text
}

// isDegradedFunctionCall 判断响应项是否为"客户端声明为 custom 却被降级成 function_call"。
func isDegradedFunctionCall(item map[string]any, customTools map[string]bool) bool {
	if item == nil || customTools == nil {
		return false
	}
	if stringifyAny(item["type"]) != "function_call" {
		return false
	}
	name := stringifyAny(item["name"])
	return name != "" && customTools[name]
}

// toCustomToolCallItem 把降级的 function_call 项改写成 custom_tool_call。
// 上游项没有 call_id 只有 id（通常 fc_call_<uuid>），id 原样作为 call_id 复用，
// 客户端回传的输出才能与上游关联。
func toCustomToolCallItem(item map[string]any) map[string]any {
	callID := stringifyAny(item["call_id"])
	if callID == "" {
		callID = stringifyAny(item["id"])
	}
	repaired := make(map[string]any, len(item)+1)
	for k, v := range item {
		repaired[k] = v
	}
	repaired["type"] = "custom_tool_call"
	repaired["call_id"] = callID
	repaired["input"] = unwrapCustomToolInput(stringifyAny(item["arguments"]))
	delete(repaired, "arguments")
	delete(repaired, "parameters")
	if id := stringifyAny(repaired["id"]); id == "" && callID != "" {
		repaired["id"] = callID
	}
	return repaired
}

// repairedTool 是一条修理记录（诊断/日志用）。
type repairedTool struct {
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
}

// repairResponsesBodyToolShapes 修理非流式 Responses body：把 output 里降级的
// function_call 改写成 custom_tool_call。返回改写后的 body 与修理记录（未改动时 json 原样、repaired 为空）。
func repairResponsesBodyToolShapes(json map[string]any, customTools map[string]bool) (map[string]any, []repairedTool) {
	if json == nil || len(customTools) == 0 {
		return json, nil
	}
	output, ok := json["output"].([]any)
	if !ok {
		return json, nil
	}
	var repaired []repairedTool
	next := make([]any, len(output))
	changed := false
	for i, item := range output {
		m, ok := item.(map[string]any)
		if !ok || !isDegradedFunctionCall(m, customTools) {
			next[i] = item
			continue
		}
		fixed := toCustomToolCallItem(m)
		next[i] = fixed
		repaired = append(repaired, repairedTool{Tool: stringifyAny(fixed["name"]), CallID: stringifyAny(fixed["call_id"])})
		changed = true
	}
	if !changed {
		return json, nil
	}
	out := make(map[string]any, len(json)+1)
	for k, v := range json {
		out[k] = v
	}
	out["output"] = next
	return out, repaired
}

// toolShapeTrackedItem 是 SSE 路径里一个被跟踪的降级 custom 工具调用的累积状态。
type toolShapeTrackedItem struct {
	item          map[string]any
	outputIndex   int
	argumentsText string
	inputDoneSent bool
}

// toolShapeRepairer 是 Responses SSE 的增量修理器，接口与 MessagesSSETransformer
// 一致（Push 返回可立即下发的文本，Flush 处理残留 pending）。
type toolShapeRepairer struct {
	customTools map[string]bool
	tracked     map[string]*toolShapeTrackedItem
	pending     string
}

// newToolShapeRepairer 构造修理器。customTools 为空时所有方法原样透传。
func newToolShapeRepairer(customTools map[string]bool) *toolShapeRepairer {
	return &toolShapeRepairer{customTools: customTools, tracked: map[string]*toolShapeTrackedItem{}}
}

// active 返回是否有需要修理的 custom 工具。
func (t *toolShapeRepairer) active() bool {
	return len(t.customTools) > 0
}

// Push 处理一个 chunk，返回可写给客户端的 SSE 文本（可能为空）。
func (t *toolShapeRepairer) Push(chunk string) string {
	if !t.active() || chunk == "" {
		return chunk
	}
	t.pending += chunk
	end := lastSSEBoundary(t.pending)
	if end < 0 {
		return ""
	}
	complete := t.pending[:end]
	t.pending = t.pending[end:]
	return t.transformEvents(complete)
}

// Flush 收尾：处理残留 pending。
func (t *toolShapeRepairer) Flush() string {
	if t.pending == "" {
		return ""
	}
	rest := t.pending
	t.pending = ""
	if !t.active() {
		return rest
	}
	return t.transformEvents(rest)
}

// transformEvents 把一段完整的 SSE 事件文本逐帧修理后重新拼装。
func (t *toolShapeRepairer) transformEvents(text string) string {
	var out strings.Builder
	for _, frame := range parseSSEFrames(text) {
		out.WriteString(t.transformEvent(frame.event, frame.data))
	}
	return out.String()
}

// transformEvent 修理一个完整 SSE 事件，返回要下发的文本（可能为空串=该帧被吸收，
// 也可能含多帧=补发的 custom_tool_call_input 事件）。
func (t *toolShapeRepairer) transformEvent(eventName, data string) string {
	// [DONE] 哨兵原样透传（JSON 解析会失败并把它变成 data: null）
	if strings.TrimSpace(data) == "[DONE]" {
		return "data: [DONE]\n\n"
	}
	if data == "" {
		return sseFrame(eventName, nil)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return sseFrame(eventName, nil)
	}
	typ := stringifyAny(payload["type"])
	outputIndex := intIndex(payload["output_index"])

	switch typ {
	case "response.output_item.added":
		item, _ := payload["item"].(map[string]any)
		if !isDegradedFunctionCall(item, t.customTools) {
			return sseFrame(eventName, payload)
		}
		fixed := toCustomToolCallItem(item)
		state := &toolShapeTrackedItem{
			item:        cloneAny(fixed).(map[string]any),
			outputIndex: outputIndex,
		}
		// 初始 input 置空，arguments 到达后再补 custom_tool_call_input 事件
		state.item["input"] = ""
		key := toolShapeKeyOf(fixed)
		t.tracked[key] = state
		next := cloneAny(payload).(map[string]any)
		next["item"] = state.item
		return sseFrame(eventName, next)

	case "response.function_call_arguments.delta":
		state, ok := t.tracked[stringifyAny(payload["item_id"])]
		if !ok {
			return sseFrame(eventName, payload)
		}
		state.argumentsText += stringifyAny(payload["delta"])
		// 增量无法逐段解包：arguments 收尾时才一次性发 input
		return ""

	case "response.function_call_arguments.done":
		itemID := stringifyAny(payload["item_id"])
		state, ok := t.tracked[itemID]
		if !ok {
			return sseFrame(eventName, payload)
		}
		if args, ok := payload["arguments"].(string); ok && args != "" {
			state.argumentsText = args
		}
		input := unwrapCustomToolInput(state.argumentsText)
		state.item["input"] = input
		state.inputDoneSent = true
		itemIDForEvent := stringifyAny(state.item["id"])
		if itemIDForEvent == "" {
			itemIDForEvent = itemID
		}
		return inputDoneFrames(itemIDForEvent, state.outputIndex, input, payload)

	case "response.output_item.done":
		item, _ := payload["item"].(map[string]any)
		if !isDegradedFunctionCall(item, t.customTools) {
			return sseFrame(eventName, payload)
		}
		fixed := toCustomToolCallItem(item)
		key := toolShapeKeyOf(fixed)
		state := t.tracked[key]
		var prefix string
		if state != nil && !state.inputDoneSent {
			state.inputDoneSent = true
			input := unwrapCustomToolInput(stringifyAny(item["arguments"]))
			state.item["input"] = input
			// 未走 arguments.done 时（上游直接发完整 item），补发 input delta/done
			prefix = inputDoneFrames(key, outputIndex, input, payload)
		}
		delete(t.tracked, key)
		next := cloneAny(payload).(map[string]any)
		next["item"] = fixed
		return prefix + sseFrame(eventName, next)

	case "response.completed", "response.incomplete", "response.failed":
		response, _ := payload["response"].(map[string]any)
		if response == nil {
			return sseFrame(eventName, payload)
		}
		repaired, _ := repairResponsesBodyToolShapes(response, t.customTools)
		if repaired == nil {
			return sseFrame(eventName, payload)
		}
		next := cloneAny(payload).(map[string]any)
		next["response"] = repaired
		return sseFrame(eventName, next)
	}
	return sseFrame(eventName, payload)
}

// inputDoneFrames 拼装 custom_tool_call_input.delta + .done 两条补发帧。
func inputDoneFrames(itemID string, outputIndex int, input string, template map[string]any) string {
	seq, _ := template["sequence_number"].(float64)
	delta := sseFrame("response.custom_tool_call_input.delta", map[string]any{
		"type":            "response.custom_tool_call_input.delta",
		"item_id":         itemID,
		"output_index":    outputIndex,
		"delta":           input,
		"sequence_number": seq,
	})
	done := sseFrame("response.custom_tool_call_input.done", map[string]any{
		"type":            "response.custom_tool_call_input.done",
		"item_id":         itemID,
		"output_index":    outputIndex,
		"input":           input,
		"sequence_number": seq,
	})
	return delta + done
}

// toolShapeKeyOf 返回跟踪键（id 优先，缺省用 call_id）。
func toolShapeKeyOf(item map[string]any) string {
	if id := stringifyAny(item["id"]); id != "" {
		return id
	}
	return stringifyAny(item["call_id"])
}

// cloneAny 深拷贝一个 JSON 值（map/slice/标量），避免改写共享上游帧。
func cloneAny(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}
