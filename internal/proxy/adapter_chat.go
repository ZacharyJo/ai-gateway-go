package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Responses ↔ Chat Completions（OpenAI 风格）协议适配。
// 用于只支持 /chat/completions 的上游（如 tu-zi 的 DeepSeek 渠道）。

// defaultChatMaxTokens 是 Chat 适配的默认 max_tokens（与 Messages 适配同值，
// 见 adapter.go 的 defaultMessagesMaxTokens）。
const defaultChatMaxTokens = defaultMessagesMaxTokens

// responsesToChatRequest 把 Responses 请求体转成 Chat Completions 请求体。
func responsesToChatRequest(doc map[string]any, opts adapterOptions) (map[string]any, error) {
	if doc == nil {
		return nil, newAdapterError("Responses request body must be JSON object")
	}
	messages, err := inputToChatMessages(doc["input"], extractSystemText(doc), opts)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"messages": messages}
	if model := doc["model"]; model != nil {
		out["model"] = model
	}
	// stream + stream_options.include_usage（Chat SSE 需要此选项才会在最后一帧带用量）
	var streamBool bool
	switch v := doc["stream"].(type) {
	case bool:
		streamBool = v
	case string:
		streamBool = v == "true" || v == "True" || v == "TRUE"
	}
	out["stream"] = streamBool
	if streamBool {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	maxTokens := defaultChatMaxTokens
	if v, ok := doc["max_output_tokens"].(float64); ok && v > 0 {
		maxTokens = int(v)
	}
	out["max_tokens"] = maxTokens
	for _, k := range []string{"temperature", "top_p"} {
		if v := doc[k]; v != nil {
			out[k] = v
		}
	}
	if v := doc["stop"]; v != nil {
		out["stop"] = v
	}
	if tools := convertToolsForChat(doc["tools"]); tools != nil {
		out["tools"] = tools
	}
	applyChatToolChoice(out, doc["tool_choice"])
	return out, nil
}

// extractSystemText 从 instructions 和 input[role=system] 提取纯文本 system 提示。
// Chat Completions 只支持字符串 system（不支持多块），多段拼接用换行；
// system 里的图片块无法承载，与正文/工具结果的媒体降级不同，这里直接丢弃（协议固有）。
func extractSystemText(doc map[string]any) string {
	var parts []string
	switch v := doc["instructions"].(type) {
	case string:
		if v != "" {
			parts = append(parts, v)
		}
	case []any:
		for _, block := range v {
			if m, ok := block.(map[string]any); ok {
				if stringifyAny(m["type"]) == "text" {
					if t := stringifyAny(m["text"]); t != "" {
						parts = append(parts, t)
					}
				}
			}
		}
	}
	if input, ok := doc["input"].([]any); ok {
		for _, item := range input {
			m, ok := item.(map[string]any)
			if !ok || stringifyAny(m["role"]) != "system" {
				continue
			}
			switch c := m["content"].(type) {
			case string:
				if c != "" {
					parts = append(parts, c)
				}
			case []any:
				for _, block := range c {
					if bm, ok := block.(map[string]any); ok && stringifyAny(bm["type"]) == "text" {
						if t := stringifyAny(bm["text"]); t != "" {
							parts = append(parts, t)
						}
					}
				}
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += "\n" + p
	}
	return result
}

// inputToChatMessages 把 Responses input 转成 Chat messages 数组。
func inputToChatMessages(input any, systemText string, opts adapterOptions) ([]any, error) {
	var messages []any
	if systemText != "" {
		messages = append(messages, map[string]any{"role": "system", "content": systemText})
	}
	if s, ok := input.(string); ok {
		messages = append(messages, map[string]any{"role": "user", "content": s})
		return messages, nil
	}
	arr, ok := input.([]any)
	if !ok {
		return nil, newAdapterError("Responses input must be a string or array for Chat adapter")
	}
	// 先扫一遍收集已配对的 function_call ID
	pairedIDs := map[string]bool{}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch stringifyAny(m["type"]) {
		case "function_call", "custom_tool_call":
			id := stringifyAny(m["call_id"])
			if id == "" {
				id = stringifyAny(m["id"])
			}
			if id != "" {
				pairedIDs[id] = true
			}
		}
	}
	// pending reasoning / tool_calls 缓冲：把「一个模型回合里的 commentary 文本消息
	// + 紧跟的 function_call」合并进同一条 Chat assistant 消息（对齐 cc-switch
	// coalesce_adjacent_commentary_with_tool_calls）。否则纯文本 assistant 单独成条，
	// Chat 模型会模仿它当成完整回合、在进度更新后停手不发工具调用。
	var pendingToolCalls []any
	pendingReasoning := ""
	lastAssistantIndex := -1
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringifyAny(m["role"]) == "system" {
			continue
		}
		// reasoning item：只入 pending，前向附挂到其后的 message / tool_calls。
		if isStateOnlyItem(item) {
			if r := extractReasoningSummaryText(m); r != "" {
				pendingReasoning = appendUniqueReasoning(pendingReasoning, r)
			}
			continue
		}
		switch stringifyAny(m["type"]) {
		case "function_call", "custom_tool_call":
			// 工具调用累积到 pending；调用项自身可能带 reasoning_content。
			if r := extractReasoningText(m); r != "" {
				pendingReasoning = appendUniqueReasoning(pendingReasoning, r)
			}
			tc, err := convertToChatMessage(m, pairedIDs, opts)
			if err != nil {
				return nil, err
			}
			if tc != nil {
				if calls, ok := tc["tool_calls"].([]any); ok {
					pendingToolCalls = append(pendingToolCalls, calls...)
				}
			}
			continue
		case "function_call_output", "custom_tool_call_output", "tool_result":
			// 工具结果先冲刷 pending 工具调用（tool 是 assistant tool-call 的配对项）。
			messages, lastAssistantIndex = flushPendingToolCalls(messages, &pendingToolCalls, &pendingReasoning, lastAssistantIndex)
			toolMsgs, hasMedia := toolOutputToChatMessages(m, pairedIDs, opts)
			messages = append(messages, toolMsgs...)
			if hasMedia {
				// 追加了合成 user 消息承载媒体：它是回合边界，重置 lastAssistantIndex，
				// 防止后续 tool_calls 越过这条 user 消息合并进更早的 commentary。
				lastAssistantIndex = -1
			}
			continue
		}
		// 非工具调用项：先冲刷 pending 工具调用（可能合并进上一条 assistant）。
		messages, lastAssistantIndex = flushPendingToolCalls(messages, &pendingToolCalls, &pendingReasoning, lastAssistantIndex)
		msg, err := convertToChatMessage(m, pairedIDs, opts)
		if err != nil {
			return nil, err
		}
		if msg == nil {
			continue
		}
		switch stringifyAny(msg["role"]) {
		case "assistant":
			// commentary assistant：把 pending reasoning 附挂，并记录索引以便后续
			// tool_calls 合并进来。
			pendingReasoning = attachReasoningToMessage(msg, pendingReasoning)
			messages = append(messages, msg)
			lastAssistantIndex = len(messages) - 1
		case "tool":
			messages = append(messages, msg)
		default:
			// user 等回合边界：pending reasoning 回溯附挂到上一条 assistant，
			// 不允许跨 user 回合泄漏。
			attachReasoningToPreviousAssistant(messages, lastAssistantIndex, &pendingReasoning)
			messages = append(messages, msg)
			lastAssistantIndex = -1
		}
	}
	// 收尾：冲刷剩余 tool_calls，再把尾部 reasoning 附挂到最后一条 assistant，
	// 最后给带 tool_calls 但缺 reasoning_content 的 assistant 补占位。
	messages, lastAssistantIndex = flushPendingToolCalls(messages, &pendingToolCalls, &pendingReasoning, lastAssistantIndex)
	attachReasoningToPreviousAssistant(messages, lastAssistantIndex, &pendingReasoning)
	backfillToolCallReasoningPlaceholders(messages)
	return messages, nil
}

// appendUniqueReasoning 按 \n\n 段去重后追加 reasoning（对齐 cc-switch
// append_unique_pending_reasoning）：已包含则跳过，避免并行 tool_calls 重复思考。
func appendUniqueReasoning(pending, reasoning string) string {
	reasoning = strings.TrimSpace(reasoning)
	if reasoning == "" {
		return pending
	}
	if pending == "" {
		return reasoning
	}
	if strings.Contains(pending, reasoning) {
		return pending
	}
	return pending + "\n\n" + reasoning
}

// flushPendingToolCalls 把 pending 工具调用落成 Chat 消息。若上一条是无 tool_calls
// 的 assistant 文本消息，则合并进去（保持同一回合）；否则新建 assistant 消息。
// 返回更新后的 messages 与 lastAssistantIndex。
func flushPendingToolCalls(messages []any, pendingToolCalls *[]any, pendingReasoning *string, lastAssistantIndex int) ([]any, int) {
	if len(*pendingToolCalls) == 0 {
		return messages, lastAssistantIndex
	}
	calls := *pendingToolCalls
	*pendingToolCalls = nil
	// 合并进相邻的纯文本 assistant 消息。
	if lastAssistantIndex >= 0 && lastAssistantIndex < len(messages) {
		if am, ok := messages[lastAssistantIndex].(map[string]any); ok {
			if stringifyAny(am["role"]) == "assistant" && !hasToolCalls(am) {
				am["tool_calls"] = calls
				*pendingReasoning = attachReasoningUnique(am, *pendingReasoning)
				return messages, lastAssistantIndex
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}
	*pendingReasoning = attachReasoningToMessage(msg, *pendingReasoning)
	messages = append(messages, msg)
	return messages, len(messages) - 1
}

func hasToolCalls(m map[string]any) bool {
	calls, ok := m["tool_calls"].([]any)
	return ok && len(calls) > 0
}

// attachReasoningToMessage 把 pending reasoning 直接写入/追加到消息的
// reasoning_content，并清空 pending。返回清空后的 pending（""）。
func attachReasoningToMessage(msg map[string]any, pending string) string {
	pending = strings.TrimSpace(pending)
	if pending == "" {
		return ""
	}
	existing := strings.TrimSpace(stringifyStr(msg["reasoning_content"]))
	if existing == "" {
		msg["reasoning_content"] = pending
	} else {
		msg["reasoning_content"] = existing + "\n\n" + pending
	}
	return ""
}

// attachReasoningUnique 按 \n\n 段去重合并 reasoning 到消息（对齐 cc-switch
// attach_pending_reasoning_to_assistant_unique），用于合并进已有消息的场景。
func attachReasoningUnique(msg map[string]any, pending string) string {
	pending = strings.TrimSpace(pending)
	if pending == "" {
		return ""
	}
	existing := strings.TrimSpace(stringifyStr(msg["reasoning_content"]))
	existingSegs := splitReasoningSegments(existing)
	var missing []string
	for _, seg := range splitReasoningSegments(pending) {
		if !containsString(existingSegs, seg) {
			missing = append(missing, seg)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	if len(existingSegs) == 0 {
		msg["reasoning_content"] = strings.Join(missing, "\n\n")
	} else {
		msg["reasoning_content"] = strings.Join(existingSegs, "\n\n") + "\n\n" + strings.Join(missing, "\n\n")
	}
	return ""
}

func splitReasoningSegments(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, seg := range strings.Split(text, "\n\n") {
		if s := strings.TrimSpace(seg); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// attachReasoningToPreviousAssistant 把尾部剩余 reasoning 回溯附挂到上一条
// assistant（对齐 cc-switch attach_pending_reasoning_to_previous_assistant）。
func attachReasoningToPreviousAssistant(messages []any, lastAssistantIndex int, pending *string) {
	reasoning := strings.TrimSpace(*pending)
	*pending = ""
	if reasoning == "" || lastAssistantIndex < 0 || lastAssistantIndex >= len(messages) {
		return
	}
	am, ok := messages[lastAssistantIndex].(map[string]any)
	if !ok || stringifyAny(am["role"]) != "assistant" {
		return
	}
	existing := strings.TrimSpace(stringifyStr(am["reasoning_content"]))
	if existing == "" {
		am["reasoning_content"] = reasoning
	} else {
		am["reasoning_content"] = existing + "\n\n" + reasoning
	}
}

// backfillToolCallReasoningPlaceholders 给带 tool_calls 但缺 reasoning_content 的
// assistant 消息补占位（对齐 cc-switch）。kimi/DeepSeek 等 thinking 模型要求带
// tool_calls 的 assistant 消息必须携带非空 reasoning_content。
func backfillToolCallReasoningPlaceholders(messages []any) {
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok || stringifyAny(m["role"]) != "assistant" || !hasToolCalls(m) {
			continue
		}
		if strings.TrimSpace(stringifyStr(m["reasoning_content"])) == "" {
			m["reasoning_content"] = "tool call"
		}
	}
}

// stringifyStr 只在值确为字符串时返回其内容，否则空串（避免把非字符串序列化成
// reasoning_content 造成误判）。
func stringifyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// convertToChatMessage 把一个 Responses input 项转成 Chat message。
func convertToChatMessage(item map[string]any, pairedIDs map[string]bool, opts adapterOptions) (map[string]any, error) {
	switch stringifyAny(item["type"]) {
	case "function_call":
		id := stringifyAny(item["call_id"])
		if id == "" {
			id = stringifyAny(item["id"])
		}
		name := stringifyAny(item["name"])
		args := item["arguments"]
		if args == nil {
			args = item["input"]
		}
		argsStr := stableJson(args)
		return map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": argsStr},
			}},
		}, nil

	case "custom_tool_call":
		id := stringifyAny(item["call_id"])
		if id == "" {
			id = stringifyAny(item["id"])
		}
		name := stringifyAny(item["name"])
		argsStr := stableJson(map[string]any{customToolInputKey: stringifyAny(item["input"])})
		return map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": argsStr},
			}},
		}, nil

	}

	// 普通 user/assistant 消息
	role := normalizeRole(item["role"])
	content := item["content"]
	switch c := content.(type) {
	case string:
		return map[string]any{"role": role, "content": c}, nil
	case []any:
		// 转成 Chat content blocks
		var blocks []any
		for _, block := range c {
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			chatBlock := convertToChatContentBlock(bm, opts)
			if chatBlock != nil {
				blocks = append(blocks, chatBlock)
			}
		}
		if len(blocks) == 0 {
			return nil, nil
		}
		// 若全是 text，可简化为字符串
		if len(blocks) == 1 {
			if bm, ok := blocks[0].(map[string]any); ok && stringifyAny(bm["type"]) == "text" {
				return map[string]any{"role": role, "content": stringifyAny(bm["text"])}, nil
			}
		}
		return map[string]any{"role": role, "content": blocks}, nil
	case nil:
		return map[string]any{"role": role, "content": ""}, nil
	}
	return map[string]any{"role": role, "content": stringifyAny(content)}, nil
}

// toolOutputToChatMessages 把 function_call_output / custom_tool_call_output / tool_result
// 转成 Chat 消息。返回 (消息列表, 是否追加了承载媒体的合成 user 消息)。
//
// Chat Completions 的 tool 消息只能是纯文本字符串。当工具结果的 output/content 是含媒体
// 块（input_image/image 等）的数组时，直接 stringifyAny 会走 fmt.Sprint 产出
// "[map[...] map[...]]" 垃圾字面量、丢失图片。这里把文本块留在 tool 消息，媒体块抽出
// 追加一条合成 user 消息（image_url 形态）承载——与 cc-switch tool_media 同思路。
func toolOutputToChatMessages(item map[string]any, pairedIDs map[string]bool, opts adapterOptions) ([]any, bool) {
	id := stringifyAny(item["call_id"])
	if id == "" {
		id = stringifyAny(item["tool_use_id"])
	}
	if id == "" {
		id = stringifyAny(item["id"])
	}
	// Responses 协议：function_call_output 用 "output"，tool_result 用 "content"
	raw := item["output"]
	if raw == nil {
		raw = item["content"]
	}

	// 数组型结果：拆出文本与媒体两部分。
	if arr, ok := raw.([]any); ok {
		textPart, mediaParts := splitToolOutputBlocks(arr, opts)
		if !pairedIDs[id] {
			// 孤儿 output：降级为 user 文本（媒体块作为附加 user 消息）。
			msgs := []any{map[string]any{"role": "user", "content": fmt.Sprintf("Tool result %s: %s", id, textPart)}}
			if len(mediaParts) > 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": mediaParts})
			}
			return msgs, len(mediaParts) > 0
		}
		msgs := []any{map[string]any{"role": "tool", "tool_call_id": id, "content": textPart}}
		if len(mediaParts) > 0 {
			// 媒体紧跟在配对 tool 消息之后，作为合成 user 消息呈现给模型。
			msgs = append(msgs, map[string]any{"role": "user", "content": mediaParts})
		}
		return msgs, len(mediaParts) > 0
	}

	// 非数组（字符串/对象/nil）：保持原有纯文本行为。
	output := stringifyAny(raw)
	if !pairedIDs[id] {
		return []any{map[string]any{"role": "user", "content": fmt.Sprintf("Tool result %s: %s", id, output)}}, false
	}
	return []any{map[string]any{"role": "tool", "tool_call_id": id, "content": output}}, false
}

// splitToolOutputBlocks 把工具结果的内容块数组拆成 (拼接后的文本, 媒体块列表)。
// 文本块（output_text/text/input_text）按出现顺序用换行拼接；媒体块（input_image/image）
// 经 convertToChatContentBlock 转成 Chat image_url 块。不支持/省略图片时按 opts 退化为文本，
// 该退化文本并入文本部分（不进媒体列表）。
func splitToolOutputBlocks(arr []any, opts adapterOptions) (string, []any) {
	var textParts []string
	var mediaParts []any
	for _, b := range arr {
		bm, ok := b.(map[string]any)
		if !ok {
			// 非对象块：字符串化并入文本，避免丢失。
			if s := stringifyAny(b); s != "" {
				textParts = append(textParts, s)
			}
			continue
		}
		switch stringifyAny(bm["type"]) {
		case "input_text", "text", "output_text":
			textParts = append(textParts, stringifyAny(bm["text"]))
		case "input_image", "image":
			chatBlock := convertToChatContentBlock(bm, opts)
			if chatBlock == nil {
				continue
			}
			if stringifyAny(chatBlock["type"]) == "image_url" {
				mediaParts = append(mediaParts, chatBlock)
			} else {
				// 退化文本（omitted/historical 提示）并入文本部分。
				textParts = append(textParts, stringifyAny(chatBlock["text"]))
			}
		default:
			// 其它块（含结构化数据）：JSON 化并入文本，保留信息不丢。
			textParts = append(textParts, jsonStringify(bm))
		}
	}
	return strings.Join(textParts, "\n"), mediaParts
}

// convertToChatContentBlock 把 Responses 内容块转成 Chat 内容块。
func convertToChatContentBlock(block map[string]any, opts adapterOptions) map[string]any {
	switch stringifyAny(block["type"]) {
	case "input_text", "text", "output_text":
		return map[string]any{"type": "text", "text": stringifyAny(block["text"])}
	case "input_image", "image":
		if !opts.SupportsImages {
			return map[string]any{"type": "text", "text": omittedImageText}
		}
		if opts.OmitImages {
			return map[string]any{"type": "text", "text": historicalImageText}
		}
		imageURL := stringifyAny(block["image_url"])
		// image_url 可能是字符串（直接 URL）或对象 {"url":"..."}
		if imageURL == "" {
			if obj, ok := block["image_url"].(map[string]any); ok {
				imageURL = stringifyAny(obj["url"])
			}
		}
		if imageURL == "" {
			if src, ok := block["source"].(map[string]any); ok {
				data := stringifyAny(src["data"])
				mediaType := stringifyAny(src["media_type"])
				if data != "" {
					imageURL = "data:" + mediaType + ";base64," + data
				}
			}
		}
		if imageURL == "" {
			return map[string]any{"type": "text", "text": omittedImageText}
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": imageURL},
		}
	}
	return nil
}

// convertToolsForChat 把 Responses tools 转成 Chat tools（function + custom 均转成 function）。
func convertToolsForChat(tools any) []any {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, t := range arr {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		switch stringifyAny(m["type"]) {
		case "function":
			fn := convertFunctionToolDefinitionForChat(m)
			if fn != nil {
				out = append(out, fn)
			}
		case "custom":
			fn := convertCustomToolDefinitionForChat(m)
			if fn != nil {
				out = append(out, fn)
			}
		}
	}
	return out
}

// convertFunctionToolDefinitionForChat 转换普通 function 工具为 Chat 格式。
func convertFunctionToolDefinitionForChat(m map[string]any) map[string]any {
	fn, _ := m["function"].(map[string]any)
	name := stringifyAny(m["name"])
	if name == "" && fn != nil {
		name = stringifyAny(fn["name"])
	}
	if name == "" {
		return nil
	}
	description := stringifyAny(m["description"])
	if description == "" && fn != nil {
		description = stringifyAny(fn["description"])
	}
	var schema any = m["parameters"]
	if schema == nil && fn != nil {
		schema = fn["parameters"]
	}
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name, "description": description, "parameters": schema,
		},
	}
}

// convertCustomToolDefinitionForChat 把 freeform 工具转成单字符串参数的 Chat function。
func convertCustomToolDefinitionForChat(m map[string]any) map[string]any {
	name := stringifyAny(m["name"])
	if name == "" {
		return nil
	}
	description := customToolDefHeading + "\n```json\n" + stableJson(m) + "\n```"
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name, "description": description,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					customToolInputKey: map[string]any{"type": "string", "description": customToolInputDesc},
				},
				"required": []any{customToolInputKey},
			},
		},
	}
}

// applyChatToolChoice 把 Responses tool_choice 映射到 Chat tool_choice。
// 与 Messages 适配的 applyToolChoice 对齐：map 形式除命名工具外还处理
// auto/required/any/none 枚举（此前只认命名工具，{"type":"none"} 不删 tools、
// {"type":"required"} 不映射，客户端禁用/强召工具时行为错乱）。
func applyChatToolChoice(out map[string]any, choice any) {
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			out["tool_choice"] = "auto"
		case "required", "any":
			out["tool_choice"] = "required"
		case "none":
			delete(out, "tools")
		}
	case map[string]any:
		name := stringifyAny(v["name"])
		if name == "" {
			if fn, ok := v["function"].(map[string]any); ok {
				name = stringifyAny(fn["name"])
			}
		}
		switch stringifyAny(v["type"]) {
		case "function", "tool", "custom":
			if name != "" {
				out["tool_choice"] = map[string]any{
					"type": "function", "function": map[string]any{"name": name},
				}
			}
		case "auto":
			out["tool_choice"] = "auto"
		case "any", "required":
			out["tool_choice"] = "required"
		case "none":
			delete(out, "tools")
		}
	}
}

// chatCompletionToResponsesBody 把 Chat Completions 非流式响应转成 Responses 格式。
func chatCompletionToResponsesBody(doc map[string]any, nowMs, createdAt int64, customTools map[string]bool) (map[string]any, error) {
	id := stringifyAny(doc["id"])
	if id == "" {
		id = fmt.Sprintf("resp_%d", nowMs)
	}
	model := stringifyAny(doc["model"])
	choices, _ := doc["choices"].([]any)
	if len(choices) == 0 {
		return nil, newAdapterError("chat completion has no choices")
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil, newAdapterError("chat completion choice is not an object")
	}
	msg, _ := choice["message"].(map[string]any)

	var output []any
	// 思考内容 → 独立 reasoning 项（不进正文）：来源 reasoning_content/reasoning 字段，
	// 或正文开头的 <think>...</think> 块。对齐 cc-switch，让思维链归 reasoning、正文保持干净。
	reasoningText := extractReasoningText(msg)
	// tool_calls → function_call / custom_tool_call
	if toolCalls, ok := msg["tool_calls"].([]any); ok {
		for _, tc := range toolCalls {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			tcID := stringifyAny(tcm["id"])
			fn, _ := tcm["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name := stringifyAny(fn["name"])
			args := stringifyAny(fn["arguments"])
			// 响应侧工具参数归一：exec_command 兼容 command/cmd、timeout/yield_time_ms
			args = normalizeExecCommandArgs(name, args)
			if customTools[name] {
				inputText := customToolInputText(parseToolInput(args))
				output = append(output, map[string]any{
					"id": "ctc_" + tcID, "type": "custom_tool_call", "status": "completed",
					"name": name, "input": inputText, "call_id": tcID,
				})
			} else {
				output = append(output, map[string]any{
					"id": tcID, "type": "function_call", "status": "completed",
					"name": name, "call_id": tcID, "arguments": args,
				})
			}
		}
	}
	// text content → message item。若正文以 <think> 开头，剥出思考归 reasoning、剩余才是正文。
	textContent := stringifyAny(msg["content"])
	if reasoning, answer, ok := splitLeadingThinkBlock(textContent); ok {
		if reasoning != "" && reasoningText == "" {
			reasoningText = reasoning
		}
		textContent = answer
	}
	// 思考项排在正文之前（Responses 协议约定 reasoning 先于 message）。
	if reasoningText != "" {
		output = append([]any{map[string]any{
			"id": "rs_" + id, "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoningText}},
		}}, output...)
	}
	if textContent != "" {
		msgItem := map[string]any{
			"id": id, "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": textContent, "annotations": []any{}}},
		}
		output = append(output, msgItem)
	}
	if len(output) == 0 {
		output = []any{map[string]any{
			"id": id, "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		}}
	}

	// stop_reason=length（max_tokens 截断）也按 completed 收尾：与 Messages 路径口径一致
	//（Codex 把 response.incomplete 当失败轮次，而截断时上游仍可能带回有用文本）。
	status := "completed"

	out := map[string]any{
		"id": "resp_" + id, "object": "response", "created_at": createdAt,
		"model": model, "status": status, "output": output,
	}
	if usage, ok := doc["usage"].(map[string]any); ok {
		u := map[string]any{
			"input_tokens":  int64val(usage["prompt_tokens"]),
			"output_tokens": int64val(usage["completion_tokens"]),
		}
		u["total_tokens"] = u["input_tokens"].(int64) + u["output_tokens"].(int64)
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			if cached := int64val(details["cached_tokens"]); cached > 0 {
				u["input_tokens_details"] = map[string]any{"cached_tokens": cached}
			}
		}
		out["usage"] = u
	}
	return out, nil
}

// int64val 把 JSON 数值转成 int64（json.Unmarshal 把数字解成 float64）。
func int64val(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// normalizeExecCommandArgs 归一 exec_command 工具的参数，兼容不同模型对命令工具的字段拼写差异
// （参考 ai-gateway responses_chat 的 normalizeToolArguments 思路）：
//   - command → cmd（有的模型发 command，有的发 cmd）
//   - timeout → yield_time_ms（钳制到 250-30000ms）
//
// 只对 exec_command 生效；其余工具原样返回。归一失败（无法解析）时原样返回，fail-open。
func normalizeExecCommandArgs(name, argsText string) string {
	if name != "exec_command" {
		return argsText
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(argsText), &input); err != nil {
		return argsText
	}
	changed := false
	// command → cmd
	if _, hasCmd := input["cmd"]; !hasCmd {
		if command, ok := input["command"].(string); ok && command != "" {
			input["cmd"] = command
			delete(input, "command")
			changed = true
		}
	}
	// timeout → yield_time_ms（钳制 250-30000）
	if _, hasYield := input["yield_time_ms"]; !hasYield {
		if timeout, ok := input["timeout"].(float64); ok && timeout > 0 {
			yield := timeout
			if yield < 250 {
				yield = 250
			}
			if yield > 30000 {
				yield = 30000
			}
			input["yield_time_ms"] = int64(yield)
			delete(input, "timeout")
			changed = true
		}
	}
	if !changed {
		return argsText
	}
	b, err := json.Marshal(input)
	if err != nil {
		return argsText
	}
	return string(b)
}

// ChatSSETransformer 把 Chat Completions 流式响应转成 Responses SSE 格式（有状态增量转换）。
type ChatSSETransformer struct {
	responseID  string
	createdAt   int64
	model       string
	customTools map[string]bool

	// 每个 tool call index → 累积状态
	toolCalls   map[int]*chatToolCallState
	textStarted bool
	createdSent bool             // response.created 帧只发一次
	msgID       string           // 文本 message item 的稳定 ID，在 .added 和 .done 两处复用
	textBuf     string           // 累积文本，收尾时写入 .done 事件
	outputIndex int              // 当前文本 output item 的下标
	nextIdx     int              // 全局 output_index 计数器（顺序分配，保证连续不跳号）
	usageTokens map[string]int64 // 累积 usage（input/output tokens）
	done        bool
	// reasoning（思考）项状态：来源 reasoning_content/reasoning 字段或内嵌 <think>。
	// 思考走独立 reasoning 输出项（summary_text），不混进正文 output_text。
	reasoningStarted bool
	reasoningDone    bool
	reasoningID      string
	reasoningBuf     string
	reasoningIndex   int
	// 内嵌 <think> 前导块的跨 chunk 探测状态（标签可能被切在多个 chunk）。
	inlineThink    inlineThinkMode
	inlineThinkBuf string
	// finishPending：finish_reason 已到但 response.completed 还没发。
	// include_usage 时 usage chunk 排在 finish chunk 之后，等它到了再补发 completed，
	// 否则 response.completed 永远缺 usage。
	finishPending bool
	finishModel   string // 挂起中的 model
}

type chatToolCallState struct {
	id      string
	name    string
	args    string
	index   int // Responses output_index
	custom  bool
	started bool
}

// newChatSSETransformer 构造转换器。
func newChatSSETransformer(model string, nowMs int64, customTools map[string]bool) *ChatSSETransformer {
	return &ChatSSETransformer{
		responseID:  fmt.Sprintf("resp_%d", nowMs),
		createdAt:   nowMs / 1000,
		model:       model,
		customTools: customTools,
		toolCalls:   map[int]*chatToolCallState{},
		outputIndex: -1,
	}
}

// Push 处理一个 SSE 数据行（data: ... 的内容部分），返回需要写给客户端的 Responses SSE 文本。
func (t *ChatSSETransformer) Push(data string) string {
	data = trimSSEData(data)
	if data == "" || data == "[DONE]" {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return ""
	}
	// usage 可能单独成帧（include_usage 的最后一个 chunk），也可能与 finish_reason 同帧，
	// 统一先累计，finish 时才能带上。
	t.flushUsage(parsed)
	choices, _ := parsed["choices"].([]any)
	if len(choices) == 0 {
		// usage-only chunk：OpenAI 把它排在 finish chunk 之后到达，
		// 此时 response.completed 已被挂起，补发之（含刚累计的 usage）。
		if t.finishPending {
			return t.emitCompleted()
		}
		return ""
	}
	if t.done || t.finishPending {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return ""
	}
	delta, _ := choice["delta"].(map[string]any)
	finishReason := stringifyAny(choice["finish_reason"])
	model := stringifyAny(parsed["model"])
	if model == "" {
		model = t.model
	}

	var out string
	// 首帧：发 response.created（只发一次）
	if !t.createdSent {
		t.createdSent = true
		out += sseFrame("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": t.responseID, "object": "response",
				"created_at": t.createdAt, "model": model, "status": "in_progress",
			},
		})
	}
	if delta != nil {
		out += t.processDelta(delta, model)
	}
	if finishReason != "" {
		out += t.flushFinish(model)
	}
	return out
}

func (t *ChatSSETransformer) processDelta(delta map[string]any, model string) string {
	var out string
	// reasoning delta（独立 reasoning_content/reasoning 字段）**先于** tool_calls 处理：
	// 同一 delta 帧若同时含工具与思考，先分配 reasoning 的 output_index，保证
	// reasoning 项排在工具调用之前（Codex 按 output_index 组装，顺序错位会张冠李戴）。
	if r := extractReasoningText(delta); r != "" {
		out += t.pushReasoningDelta(r)
	}
	// tool_calls delta
	if toolCallsRaw, ok := delta["tool_calls"].([]any); ok {
		// 工具调用是回合边界：先冲刷未决的内嵌 <think> 并收尾 reasoning，
		// 保证 reasoning 项排在工具调用项之前。
		out += t.flushInlineThink()
		out += t.finalizeReasoning()
		for _, tc := range toolCallsRaw {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			idx := int(int64val(tcm["index"]))
			st, exists := t.toolCalls[idx]
			if !exists {
				st = &chatToolCallState{index: t.allocOutputIndex()}
				t.toolCalls[idx] = st
			}
			// 首次出现 id/name
			tcID := stringifyAny(tcm["id"])
			if tcID != "" && st.id == "" {
				st.id = tcID
			}
			fn, _ := tcm["function"].(map[string]any)
			if fn != nil {
				if name := stringifyAny(fn["name"]); name != "" && st.name == "" {
					st.name = name
					st.custom = t.customTools[name]
				}
				st.args += stringifyAny(fn["arguments"])
			}
			if !st.started && st.name != "" {
				st.started = true
				itemID := st.id
				if st.custom {
					itemID = "ctc_" + st.id
				}
				item := map[string]any{
					"id": itemID, "type": "function_call", "status": "in_progress",
					"name": st.name, "call_id": st.id, "arguments": "",
				}
				if st.custom {
					item["type"] = "custom_tool_call"
					delete(item, "arguments")
					item["input"] = ""
				}
				out += sseFrame("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": st.index, "item": item,
				})
			}
			// exec_command 的参数会在收尾时被 normalizeExecCommandArgs 改写字段名
			// （command→cmd、timeout→yield_time_ms）。若逐块转发原始 delta，delta 流与 .done
			// 的最终 arguments 会字段不一致；靠拼 delta 重建参数的客户端会用错字段。
			// 故对会被归一的 exec_command 抑制增量 delta，只在 .done 里给出归一后的完整参数。
			// 只在 started（.added 已发）后发 .delta：name 被截断、从未 .added 的 tool call
			// 不发任何帧（与 flushFinish 的 started 守卫对齐，避免孤儿 delta）。
			if st.started && fn != nil && stringifyAny(fn["arguments"]) != "" && !st.custom && st.name != "exec_command" {
				out += sseFrame("response.function_call_arguments.delta", map[string]any{
					"type":    "response.function_call_arguments.delta",
					"item_id": st.id, "output_index": st.index,
					"delta": stringifyAny(fn["arguments"]),
				})
			}
		}
	}
	// 正文 delta：可能内嵌 <think> 前导块，用状态机跨 chunk 剥离到 reasoning。
	if content := stringifyAny(delta["content"]); content != "" {
		out += t.pushContentDelta(content)
	}
	return out
}

// pushContentDelta 处理正文增量，剥离前导 <think> 块（跨 chunk 状态机）。
// <think> 块内容归 reasoning，闭合后的正文才进 output_text。
func (t *ChatSSETransformer) pushContentDelta(delta string) string {
	switch t.inlineThink {
	case inlineThinkText:
		return t.pushTextDelta(delta)
	case inlineThinkDetecting:
		t.inlineThinkBuf += delta
		switch leadingThinkPrefixDecision(t.inlineThinkBuf) {
		case thinkNeedMore:
			return "" // 继续攒，标签可能还没到齐
		case thinkReasoning:
			t.inlineThink = inlineThinkReasoning
			return t.drainCompleteInlineThink()
		default: // thinkText：确认不是 <think>，缓冲的全部当正文
			t.inlineThink = inlineThinkText
			text := t.inlineThinkBuf
			t.inlineThinkBuf = ""
			return t.pushTextDelta(text)
		}
	case inlineThinkReasoning:
		t.inlineThinkBuf += delta
		return t.drainCompleteInlineThink()
	}
	return ""
}

// drainCompleteInlineThink 尝试从缓冲里切出完整 <think>...</think>：
// 命中则思考归 reasoning、剩余正文进 output_text，并切到 Text 模式；未命中继续缓冲。
func (t *ChatSSETransformer) drainCompleteInlineThink() string {
	reasoning, answer, ok := splitLeadingThinkBlock(t.inlineThinkBuf)
	if !ok {
		return "" // </think> 还没到，继续缓冲
	}
	t.inlineThink = inlineThinkText
	t.inlineThinkBuf = ""
	var out string
	if reasoning != "" {
		out += t.pushReasoningDelta(reasoning)
		out += t.finalizeReasoning()
	}
	if answer != "" {
		out += t.pushTextDelta(answer)
	}
	return out
}

// flushInlineThink 在流收尾/边界（tool_calls 到达或最终 Flush）冲刷缓冲的未决 <think> 内容。
func (t *ChatSSETransformer) flushInlineThink() string {
	switch t.inlineThink {
	case inlineThinkText:
		return ""
	case inlineThinkDetecting:
		t.inlineThink = inlineThinkText
		text := t.inlineThinkBuf
		t.inlineThinkBuf = ""
		if text == "" {
			return ""
		}
		return t.pushTextDelta(text)
	case inlineThinkReasoning:
		buffered := t.inlineThinkBuf
		t.inlineThinkBuf = ""
		t.inlineThink = inlineThinkText
		if reasoning, answer, ok := splitLeadingThinkBlock(buffered); ok {
			var out string
			if reasoning != "" {
				out += t.pushReasoningDelta(reasoning)
				out += t.finalizeReasoning()
			}
			if answer != "" {
				out += t.pushTextDelta(answer)
			}
			return out
		}
		// 无闭合 </think>：缓冲可能是思考被截断（max_tokens），也可能是漏了闭合标签但答案
		// 已跟在后面。两种情况都无法可靠切分，一律去开标签后当正文兜底下发（并清洗残留
		// think 标记），保证这一轮不会只剩 reasoning 而空手结束——codex 对没有 message
		// 的完成轮会当作一轮没执行完就提前收尾。
		text, _ := stripLeadingThinkOpenTag(buffered)
		if text == "" {
			return ""
		}
		return t.pushTextDelta(sanitizeAssistantText(text))
	}
	return ""
}

// pushReasoningDelta 输出一段思考增量到独立 reasoning 项（首帧补 item/part added）。
func (t *ChatSSETransformer) pushReasoningDelta(delta string) string {
	// reasoning 项已收尾（finalizeReasoning 发过 output_item.done）后不能再对它追加 delta：
	// 否则是对已完成 item 的协议违规写入，且这段 delta 永远等不到匹配的 .done。丢弃迟到的 reasoning。
	if t.reasoningDone {
		return ""
	}
	var out string
	if !t.reasoningStarted {
		t.reasoningStarted = true
		t.reasoningIndex = t.allocOutputIndex()
		t.reasoningID = fmt.Sprintf("rs_%d", time.Now().UnixMilli())
		out += sseFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": t.reasoningIndex,
			"item": map[string]any{
				"id": t.reasoningID, "type": "reasoning", "status": "in_progress", "summary": []any{},
			},
		})
		out += sseFrame("response.reasoning_summary_part.added", map[string]any{
			"type":    "response.reasoning_summary_part.added",
			"item_id": t.reasoningID, "output_index": t.reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	t.reasoningBuf += delta
	out += sseFrame("response.reasoning_summary_text.delta", map[string]any{
		"type":    "response.reasoning_summary_text.delta",
		"item_id": t.reasoningID, "output_index": t.reasoningIndex, "summary_index": 0,
		"delta": delta,
	})
	return out
}

// finalizeReasoning 收尾当前 reasoning 项（summary_text.done → summary_part.done → output_item.done）。
// 已收尾或未开始则空操作。
func (t *ChatSSETransformer) finalizeReasoning() string {
	if !t.reasoningStarted || t.reasoningDone {
		return ""
	}
	t.reasoningDone = true
	out := sseFrame("response.reasoning_summary_text.done", map[string]any{
		"type":    "response.reasoning_summary_text.done",
		"item_id": t.reasoningID, "output_index": t.reasoningIndex, "summary_index": 0,
		"text": t.reasoningBuf,
	})
	out += sseFrame("response.reasoning_summary_part.done", map[string]any{
		"type":    "response.reasoning_summary_part.done",
		"item_id": t.reasoningID, "output_index": t.reasoningIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": t.reasoningBuf},
	})
	out += sseFrame("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": t.reasoningIndex,
		"item": map[string]any{
			"id": t.reasoningID, "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": t.reasoningBuf}},
		},
	})
	return out
}

// pushTextDelta 输出一段正文增量到 message 项（首帧补 item/part added）。
func (t *ChatSSETransformer) pushTextDelta(delta string) string {
	var out string
	if !t.textStarted {
		// 正文开始前先收尾 reasoning（来自独立字段或已剥离的 <think>），保证 reasoning 完整先于正文。
		out += t.finalizeReasoning()
		t.textStarted = true
		t.outputIndex = t.allocOutputIndex()
		t.msgID = fmt.Sprintf("msg_%d", time.Now().UnixMilli())
		out += sseFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": t.outputIndex,
			"item": map[string]any{
				"id": t.msgID, "type": "message", "status": "in_progress", "role": "assistant",
				"content": []any{},
			},
		})
		out += sseFrame("response.content_part.added", map[string]any{
			"type":    "response.content_part.added",
			"item_id": t.msgID, "output_index": t.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	t.textBuf += delta
	out += sseFrame("response.output_text.delta", map[string]any{
		"type":         "response.output_text.delta",
		"item_id":      t.msgID, // 带上 item_id，与 Messages 转换器及本路径的 content_part 帧一致
		"output_index": t.outputIndex, "content_index": 0, "delta": delta,
	})
	return out
}

func (t *ChatSSETransformer) flushFinish(model string) string {
	var out string
	// 收尾前冲刷未决的内嵌 <think> 缓冲，并收尾 reasoning 项（若还开着）。
	out += t.flushInlineThink()
	out += t.finalizeReasoning()
	// 收尾各 tool call
	// 按 output_index 升序收尾：map 迭代顺序随机，直接 range 会让并行工具的
	// output_item.done 帧乱序（Codex 侧按顺序拼接，乱序会错配 call）。
	stateOrder := make([]*chatToolCallState, 0, len(t.toolCalls))
	for _, st := range t.toolCalls {
		stateOrder = append(stateOrder, st)
	}
	sort.Slice(stateOrder, func(i, j int) bool { return stateOrder[i].index < stateOrder[j].index })
	for _, st := range stateOrder {
		// 只有 index/name/id 分片、从未触发过 .added 的 tool call 不发收尾帧：
		// 否则给客户端一个没有 output_item.added 配对的孤儿 .done（上游流中途断掉时）。
		if !st.started {
			continue
		}
		// 流式累积的 args 是分片拼接的 JSON，收尾时归一 exec_command 参数
		argsNorm := normalizeExecCommandArgs(st.name, st.args)
		if st.custom {
			inputText := customToolInputText(parseToolInput(argsNorm))
			out += sseFrame("response.custom_tool_call_input.done", map[string]any{
				"type":    "response.custom_tool_call_input.done",
				"item_id": "ctc_" + st.id, "output_index": st.index, "input": inputText,
			})
			out += sseFrame("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": st.index,
				"item": map[string]any{
					"id": "ctc_" + st.id, "type": "custom_tool_call", "status": "completed",
					"name": st.name, "call_id": st.id, "input": inputText,
				},
			})
		} else {
			out += sseFrame("response.function_call_arguments.done", map[string]any{
				"type":    "response.function_call_arguments.done",
				"item_id": st.id, "output_index": st.index, "arguments": argsNorm,
			})
			out += sseFrame("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": st.index,
				"item": map[string]any{
					"id": st.id, "type": "function_call", "status": "completed",
					"name": st.name, "call_id": st.id, "arguments": argsNorm,
				},
			})
		}
	}
	// 收尾文本：用累积的 textBuf 写入 .done 事件，保证 Codex 能看到完整内容
	if t.textStarted {
		out += sseFrame("response.content_part.done", map[string]any{
			"type":    "response.content_part.done",
			"item_id": t.msgID, "output_index": t.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": t.textBuf, "annotations": []any{}},
		})
		out += sseFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": t.outputIndex,
			"item": map[string]any{
				"id": t.msgID, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": t.textBuf, "annotations": []any{}}},
			},
		})
	}
	// stop_reason=length（max_tokens 截断）也按 completed 收尾：与 Messages 路径
	//（adapter.go messagesToResponsesBody）口径一致——Codex 把 response.incomplete 当
	// 失败轮次处理，而截断时上游仍可能带回有用文本。终态恒 completed。
	// response.completed/[DONE] 先不发：include_usage 时 usage chunk 在 finish 之后才到，
	// 等它（或 Flush 收尾）再补发，否则流式响应的 usage 永远到不了客户端。
	t.finishPending = true
	t.finishModel = model
	return out
}

// emitCompleted 补发被挂起的 response.completed + [DONE]（finish 之后等 usage chunk 或 Flush）。
func (t *ChatSSETransformer) emitCompleted() string {
	t.done = true
	t.finishPending = false
	resp := map[string]any{
		"id": t.responseID, "object": "response",
		"created_at": t.createdAt, "model": t.finishModel, "status": "completed",
	}
	if len(t.usageTokens) > 0 {
		in := t.usageTokens["input_tokens"]
		out2 := t.usageTokens["output_tokens"]
		resp["usage"] = map[string]any{
			"input_tokens": in, "output_tokens": out2, "total_tokens": in + out2,
		}
	}
	return sseFrame("response.completed", map[string]any{
		"type": "response.completed", "response": resp,
	}) + "data: [DONE]\n\n"
}

func (t *ChatSSETransformer) flushUsage(parsed map[string]any) {
	if usage, ok := parsed["usage"].(map[string]any); ok {
		if t.usageTokens == nil {
			t.usageTokens = map[string]int64{}
		}
		t.usageTokens["input_tokens"] = int64val(usage["prompt_tokens"])
		t.usageTokens["output_tokens"] = int64val(usage["completion_tokens"])
	}
}

// BufferingThink 报告当前是否处于内嵌 <think> 缓冲期。此期间转换器对下游零输出
// （思考内容要等 </think> 闭合才下发），流循环据此决定是否发保活注释帧。
func (t *ChatSSETransformer) BufferingThink() bool {
	return t.inlineThink == inlineThinkReasoning
}

func (t *ChatSSETransformer) Flush() string {
	if t.done {
		return ""
	}
	if t.finishPending {
		// 流已干净结束但没等到 usage chunk（上游未开 include_usage）：补发挂起的 completed
		return t.emitCompleted()
	}
	// 无 finish_reason 直接结束：冲刷未决的内嵌 <think> 与 reasoning，再收尾。
	out := t.flushInlineThink()
	out += t.finalizeReasoning()
	t.done = true
	resp := map[string]any{
		"id": t.responseID, "object": "response",
		"created_at": t.createdAt, "model": t.model, "status": "completed",
	}
	// 与 emitCompleted 一致：上游给了 usage 就带上（否则该分支把 usage 丢掉，计量漏计）。
	if t.usageTokens != nil {
		in := t.usageTokens["input_tokens"]
		outN := t.usageTokens["output_tokens"]
		resp["usage"] = map[string]any{"input_tokens": in, "output_tokens": outN, "total_tokens": in + outN}
	}
	return out + sseFrame("response.completed", map[string]any{
		"type": "response.completed", "response": resp,
	}) + "data: [DONE]\n\n"
}

// allocOutputIndex 分配下一个 output_index（全局递增，保证连续不跳号）。
func (t *ChatSSETransformer) allocOutputIndex() int {
	idx := t.nextIdx
	t.nextIdx++
	return idx
}

// Usage 返回累积的 input/output token 数（计量用；Flush 后读取）。
// 上游未开 include_usage（无 usage chunk）时用流式文本做 token 估算兜底，
// 避免整请求按 0 计费。
func (t *ChatSSETransformer) Usage() (in, out int64) {
	if t.usageTokens != nil {
		return t.usageTokens["input_tokens"], t.usageTokens["output_tokens"]
	}
	if t.textBuf != "" {
		return 0, int64(estimateTokens(t.textBuf))
	}
	return 0, 0
}

// trimSSEData 去掉 "data: " 前缀（Push 接收原始行或纯 data 均可）。
func trimSSEData(s string) string {
	if strings.HasPrefix(s, "data: ") {
		return s[len("data: "):]
	}
	return s
}
