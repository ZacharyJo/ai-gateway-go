package proxy

import (
	"encoding/json"
	"fmt"
	"time"
)

// Responses ↔ Chat Completions（OpenAI 风格）协议适配。
// 用于只支持 /chat/completions 的上游（如 tu-zi 的 DeepSeek 渠道）。

const defaultChatMaxTokens = 8192

// responsesToChatRequest 把 Responses 请求体转成 Chat Completions 请求体。
func responsesToChatRequest(doc map[string]any, opts adapterOptions) (map[string]any, error) {
	if doc == nil {
		return nil, newAdapterError("Responses request body must be JSON object")
	}
	messages, err := inputToChatMessages(doc["input"], extractSystemText(doc, opts), opts)
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
// Chat Completions 只支持字符串 system（不支持多块），多段拼接用换行。
func extractSystemText(doc map[string]any, opts adapterOptions) string {
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
	_ = opts
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
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if isStateOnlyItem(item) || stringifyAny(m["role"]) == "system" {
			continue
		}
		msg, err := convertToChatMessage(m, pairedIDs, opts)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			messages = append(messages, msg)
		}
	}
	return messages, nil
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

	case "function_call_output", "custom_tool_call_output", "tool_result":
		id := stringifyAny(item["call_id"])
		if id == "" {
			id = stringifyAny(item["tool_use_id"])
		}
		if id == "" {
			id = stringifyAny(item["id"])
		}
		// Responses 协议：function_call_output 用 "output"，tool_result 用 "content"
		output := stringifyAny(item["output"])
		if output == "" {
			output = stringifyAny(item["content"])
		}
		if !pairedIDs[id] {
			// 孤儿 output：降级为 user 文本
			return map[string]any{"role": "user", "content": fmt.Sprintf("Tool result %s: %s", id, output)}, nil
		}
		return map[string]any{"role": "tool", "tool_call_id": id, "content": output}, nil
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
		if name != "" {
			out["tool_choice"] = map[string]any{
				"type": "function", "function": map[string]any{"name": name},
			}
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
	// text content → message item
	// DeepSeek 等模型开启思维链时 content="" 而实际内容在 reasoning_content，取非空的那个
	textContent := stringifyAny(msg["content"])
	if textContent == "" {
		textContent = stringifyAny(msg["reasoning_content"])
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

	status := "completed"
	var incompleteDetails map[string]any
	if stringifyAny(choice["finish_reason"]) == "length" {
		status = "incomplete"
		incompleteDetails = map[string]any{"reason": "max_output_tokens"}
	}

	out := map[string]any{
		"id": "resp_" + id, "object": "response", "created_at": createdAt,
		"model": model, "status": status, "output": output,
	}
	if incompleteDetails != nil {
		out["incomplete_details"] = incompleteDetails
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
// 参考 normalizeToolArguments 的归一逻辑：
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
	// finishPending：finish_reason 已到但 response.completed 还没发。
	// include_usage 时 usage chunk 排在 finish chunk 之后，等它到了再补发 completed，
	// 否则 response.completed 永远缺 usage。
	finishPending bool
	finishStatus  string // 挂起中的终态（completed / incomplete）
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
		out += t.flushFinish(finishReason, model)
	}
	return out
}

func (t *ChatSSETransformer) processDelta(delta map[string]any, model string) string {
	var out string
	// tool_calls delta
	if toolCallsRaw, ok := delta["tool_calls"].([]any); ok {
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
			if fn != nil && stringifyAny(fn["arguments"]) != "" && !st.custom {
				out += sseFrame("response.function_call_arguments.delta", map[string]any{
					"type":    "response.function_call_arguments.delta",
					"item_id": st.id, "output_index": st.index,
					"delta": stringifyAny(fn["arguments"]),
				})
			}
		}
	}
	// text content delta
	// DeepSeek 思维链模型：content="" 而内容在 reasoning_content，两者取非空的
	content := stringifyAny(delta["content"])
	if content == "" {
		content = stringifyAny(delta["reasoning_content"])
	}
	if content != "" {
		if !t.textStarted {
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
		t.textBuf += content
		out += sseFrame("response.output_text.delta", map[string]any{
			"type":         "response.output_text.delta",
			"output_index": t.outputIndex, "content_index": 0, "delta": content,
		})
	}
	return out
}

func (t *ChatSSETransformer) flushFinish(finishReason, model string) string {
	var out string
	// 收尾各 tool call
	for _, st := range t.toolCalls {
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
	status := "completed"
	if finishReason == "length" {
		status = "incomplete"
	}
	// response.completed/[DONE] 先不发：include_usage 时 usage chunk 在 finish 之后才到，
	// 等它（或 Flush 收尾）再补发，否则流式响应的 usage 永远到不了客户端。
	t.finishPending = true
	t.finishStatus = status
	t.finishModel = model
	return out
}

// emitCompleted 补发被挂起的 response.completed + [DONE]（finish 之后等 usage chunk 或 Flush）。
func (t *ChatSSETransformer) emitCompleted() string {
	t.done = true
	t.finishPending = false
	resp := map[string]any{
		"id": t.responseID, "object": "response",
		"created_at": t.createdAt, "model": t.finishModel, "status": t.finishStatus,
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

func (t *ChatSSETransformer) Flush() string {
	if t.done {
		return ""
	}
	if t.finishPending {
		// 流已干净结束但没等到 usage chunk（上游未开 include_usage）：补发挂起的 completed
		return t.emitCompleted()
	}
	t.done = true
	return sseFrame("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": t.responseID, "object": "response",
			"created_at": t.createdAt, "model": t.model, "status": "completed",
		},
	}) + "data: [DONE]\n\n"
}

// allocOutputIndex 分配下一个 output_index（全局递增，保证连续不跳号）。
func (t *ChatSSETransformer) allocOutputIndex() int {
	idx := t.nextIdx
	t.nextIdx++
	return idx
}

// trimSSEData 去掉 "data: " 前缀（Push 接收原始行或纯 data 均可）。
func trimSSEData(s string) string {
	if len(s) > 6 && s[:6] == "data: " {
		return s[6:]
	}
	return s
}
