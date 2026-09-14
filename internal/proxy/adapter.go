package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Responses ↔ Messages（Anthropic 风格）协议适配。
// 非 GPT 模型的 /v1/responses 请求转成 Messages 格式打上游 /messages，响应再转回 Responses 格式。

// AdapterError 是适配失败错误，带 HTTP 状态码与错误类型（对应 AdapterError）。
type AdapterError struct {
	Message string
	Status  int
	Type    string
}

func (e *AdapterError) Error() string { return e.Message }

// newAdapterError 构造默认 400 的适配错误。
func newAdapterError(format string, args ...any) *AdapterError {
	return &AdapterError{Message: fmt.Sprintf(format, args...), Status: 400, Type: "responses_messages_adapter_error"}
}

const (
	defaultMessagesMaxTokens = 8192
	maxMessagesImageBytes    = 5 * 1024 * 1024
	omittedImageText         = "[图片内容已省略，当前模型不支持图片输入]"
	oversizedImageText       = "[图片内容已省略，图片超过 Messages 接口 5MB 限制]"
	historicalImageText      = "[历史图片内容已省略，继续对话时不重复带入历史图片]"
)

// adapterOptions 是转换选项（对应 JS 的 options 对象）。
type adapterOptions struct {
	SupportsImages bool
	OmitImages     bool // 非最后一条输入的图片一律省略
}

// asContentArray 把 content 归一为数组（字符串包成 input_text；nil 为空）。
func asContentArray(content any) ([]any, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		return []any{map[string]any{"type": "input_text", "text": v}}, nil
	case []any:
		return v, nil
	default:
		return nil, newAdapterError("unsupported Responses content value: %T", content)
	}
}

// normalizeRole 归一角色：assistant/system 保留，其余归 user。
func normalizeRole(role any) string {
	switch stringifyAny(role) {
	case "assistant":
		return "assistant"
	case "system":
		return "system"
	default:
		return "user"
	}
}

// isStateOnlyItem 判断是否为"纯状态载体"（reasoning / encrypted_content），
// Messages 协议没有对应载体，必然丢弃。
func isStateOnlyItem(item any) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	t := stringifyAny(m["type"])
	if t == "reasoning" || t == "encrypted_content" {
		return true
	}
	_, has := m["encrypted_content"]
	return has
}

// parseToolInput 解析工具入参：对象原样；字符串尝试 JSON，失败包成 {value}。
func parseToolInput(value any) any {
	switch v := value.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return v
	case []any:
		return v
	case string:
		if v == "" {
			return map[string]any{}
		}
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			if _, ok := parsed.(map[string]any); ok {
				return parsed
			}
			if _, ok := parsed.([]any); ok {
				return parsed
			}
			return map[string]any{"value": parsed}
		}
		return map[string]any{"value": v}
	default:
		return map[string]any{"value": v}
	}
}

// stableJson 序列化（nil → {}），对应 stableJson。
func stableJson(value any) string {
	if value == nil {
		return "{}"
	}
	return jsonStringify(value)
}

// 旧版工具调用转录的中性化正则。调用格式与结果格式各一条，两者都用于**实际替换**，
// 不再单独维护一条"检测"正则 —— 检测与替换写法一旦不一致（例如检测不要求括号前有空格、
// 替换要求），就会出现"命中了却没替换、只加了个外壳"的情况。
// 括号前的空格可有可无，括号内允许为空。
var legacyToolCallReplace = regexp.MustCompile(`Tool call call_[^\s(]+\s*\(([^)]*)\):`)
var legacyToolResultReplace = regexp.MustCompile(`Tool result call_[^\s:]+:`)

const legacyToolCallHeading = "[历史工具调用文本记录，仅供理解上下文，不要复述或模仿此格式]"

// neutralizeLegacyToolCallText 把历史"Tool call call_xxx(...)"文本中性化，避免模型模仿该格式。
// 以"替换是否真的改动了内容"作为命中判据：没改动就返回空串，调用方保持原文。
func neutralizeLegacyToolCallText(text string) string {
	neutralized := legacyToolCallReplace.ReplaceAllString(text, "历史工具调用 $1：")
	if neutralized == text {
		return ""
	}
	neutralized = legacyToolResultReplace.ReplaceAllString(neutralized, "历史工具返回结果：")
	return legacyToolCallHeading + "\n" + neutralized
}

// neutralizeAssistantTextBlocks 逐块中性化 assistant 内容里的旧版工具调用转录。
// 只改命中的文本块：整条消息替换成单个文本块会把同消息里的 tool_use 一起丢掉，
// 与之配对的 tool_result 就变成孤儿。
func neutralizeAssistantTextBlocks(content []any) []any {
	out := make([]any, 0, len(content))
	for _, block := range content {
		switch b := block.(type) {
		case string:
			if n := neutralizeLegacyToolCallText(b); n != "" {
				out = append(out, n)
				continue
			}
		case map[string]any:
			if t := stringifyAny(b["type"]); t == "text" || t == "output_text" {
				if n := neutralizeLegacyToolCallText(stringifyAny(b["text"])); n != "" {
					rewritten := make(map[string]any, len(b))
					for k, v := range b {
						rewritten[k] = v
					}
					rewritten["text"] = n
					out = append(out, rewritten)
					continue
				}
			}
		}
		out = append(out, block)
	}
	return out
}

var minimaxLeakPattern = regexp.MustCompile(`(?i)(?:\s*[\]|｜]?<\]minimax\[>\[)+`)

// sanitizeAssistantText 去掉 minimax 模型泄漏的内部标记。
func sanitizeAssistantText(text string) string {
	return minimaxLeakPattern.ReplaceAllString(text, "")
}

// freeform（type=custom）工具在 Messages 侧的承载方式。
// Messages 协议没有 grammar 约束的自由文本工具，只能包一层单字符串参数，
// 把原始定义（含 lark 语法）原样嵌进 description 让模型照格式产出，回程再拆出来。
// codex 的 apply_patch 属于这一类：不转换的话工具定义会被丢掉，模型无法写文件。
const (
	customToolInputKey   = "input"
	customToolInputDesc  = "原始 freeform 工具的纯文本输入。严格保留格式，不要包成 JSON，按 description 里嵌入的原始工具定义产出。"
	customToolDefHeading = "Original tool definition:"
)

// convertToolDefinition 把 Responses 的工具定义转成 Messages 的 input_schema 形式。
// function 直接映射；custom（freeform）包成单字符串参数；其余类型（tool_search/web_search
// 等托管工具）Messages 侧没有对应载体，返回 nil 丢弃。
func convertToolDefinition(tool any) map[string]any {
	m, ok := tool.(map[string]any)
	if !ok {
		return nil
	}
	switch stringifyAny(m["type"]) {
	case "function":
		return convertFunctionToolDefinition(m)
	case "custom":
		return convertCustomToolDefinition(m)
	}
	return nil
}

// convertFunctionToolDefinition 转换普通 function 工具。
func convertFunctionToolDefinition(m map[string]any) map[string]any {
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
	return map[string]any{"name": name, "description": description, "input_schema": schema}
}

// convertCustomToolDefinition 把 freeform 工具转成单字符串参数的 Messages 工具。
// 原始定义（含 format.definition 的 lark 语法）整体序列化进 description：
// 上游不认 grammar 约束，只能靠提示词传达格式要求。
func convertCustomToolDefinition(m map[string]any) map[string]any {
	name := stringifyAny(m["name"])
	if name == "" {
		return nil
	}
	description := customToolDefHeading + "\n```json\n" + stableJson(m) + "\n```"
	return map[string]any{
		"name": name, "description": description,
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				customToolInputKey: map[string]any{"type": "string", "description": customToolInputDesc},
			},
			"required": []any{customToolInputKey},
		},
	}
}

// collectCustomToolNames 收集请求里声明为 freeform（type=custom）的工具名。
// 回程要靠它把 tool_use 还原成 custom_tool_call 而不是 function_call —— 两者
// 在 Responses 协议里的载荷不同（顶层 input 纯文本 vs arguments JSON 串）。
func collectCustomToolNames(tools any) map[string]bool {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	var out map[string]bool
	for _, t := range arr {
		m, ok := t.(map[string]any)
		if !ok || stringifyAny(m["type"]) != "custom" {
			continue
		}
		if name := stringifyAny(m["name"]); name != "" {
			if out == nil {
				out = map[string]bool{}
			}
			out[name] = true
		}
	}
	return out
}

// convertTools 转换工具列表，无有效工具返回 nil。
func convertTools(tools any) []any {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, t := range arr {
		if converted := convertToolDefinition(t); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

// toolUseToOutputItem 把 Messages 的 tool_use 块转成 Responses 输出项。
// 名字在 customTools 里的还原成 custom_tool_call（顶层 input 纯文本），否则 function_call。
func toolUseToOutputItem(block map[string]any, nowMs int64, customTools map[string]bool) map[string]any {
	if name := stringifyAny(block["name"]); name != "" && customTools[name] {
		return toolUseToCustomToolCall(block, nowMs, name)
	}
	return toolUseToFunctionCall(block, nowMs)
}

// toolUseToCustomToolCall 把 tool_use 还原成 custom_tool_call 项。
func toolUseToCustomToolCall(block map[string]any, nowMs int64, name string) map[string]any {
	callID := stringifyAny(block["id"])
	if callID == "" {
		callID = fmt.Sprintf("toolu_%d", nowMs)
	}
	id := callID
	if !strings.HasPrefix(id, "ctc_") {
		id = "ctc_" + id
	}
	return map[string]any{
		"id": id, "type": "custom_tool_call", "status": "completed",
		"name": name, "input": customToolInputText(block["input"]), "call_id": callID,
	}
}

// customToolInputText 从 tool_use.input 取出 freeform 工具的纯文本输入。
// 正常是 {"input":"..."} 的包装；模型没照格式包时退回整体序列化，避免丢内容。
func customToolInputText(input any) string {
	if m, ok := input.(map[string]any); ok {
		if v, has := m[customToolInputKey]; has {
			return stringifyAny(v)
		}
		return stableJson(m)
	}
	return stringifyAny(input)
}

// toolUseToFunctionCall 把 Messages 的 tool_use 块转成 Responses 的 function_call 项。
func toolUseToFunctionCall(block map[string]any, nowMs int64) map[string]any {
	callID := stringifyAny(block["id"])
	if callID == "" {
		callID = fmt.Sprintf("toolu_%d", nowMs)
	}
	id := callID
	if !strings.HasPrefix(id, "fc_") {
		id = "fc_" + id
	}
	name := stringifyAny(block["name"])
	if name == "" {
		name = "tool"
	}
	return map[string]any{
		"id": id, "type": "function_call", "status": "completed",
		"name": name, "arguments": stableJson(block["input"]), "call_id": callID,
	}
}

var dataURLPattern = regexp.MustCompile(`(?i)^data:([^;,]+);base64,([A-Za-z0-9+/=\r\n_-]+)$`)
var whitespacePattern = regexp.MustCompile(`\s+`)

// parseDataUrlImage 解析 data URL 图片，返回 (mediaType, base64)。
func parseDataUrlImage(value string) (string, string, bool) {
	m := dataURLPattern.FindStringSubmatch(value)
	if m == nil {
		return "", "", false
	}
	return m[1], whitespacePattern.ReplaceAllString(m[2], ""), true
}

// base64DecodedBytes 估算 base64 解码后的字节数。
func base64DecodedBytes(base64Str string) int {
	normalized := strings.NewReplacer("-", "+", "_", "/").Replace(base64Str)
	padding := 0
	if strings.HasSuffix(normalized, "==") {
		padding = 2
	} else if strings.HasSuffix(normalized, "=") {
		padding = 1
	}
	return max(0, len(normalized)*3/4-padding)
}

// convertBase64Image 转 Messages 图片块；超过 5MB 限制时降级为占位文本。
func convertBase64Image(mediaType, data string) map[string]any {
	normalized := whitespacePattern.ReplaceAllString(data, "")
	if base64DecodedBytes(normalized) > maxMessagesImageBytes {
		return map[string]any{"type": "text", "text": oversizedImageText}
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": normalized}}
}

// convertContentBlock 转换单个内容块。返回 (nil, nil) 表示跳过（纯状态项）。
func convertContentBlock(block any, opts adapterOptions) (map[string]any, error) {
	if s, ok := block.(string); ok {
		return map[string]any{"type": "text", "text": s}, nil
	}
	m, ok := block.(map[string]any)
	if !ok {
		return nil, newAdapterError("unsupported Responses content block")
	}
	switch stringifyAny(m["type"]) {
	case "input_text", "text", "output_text":
		return map[string]any{"type": "text", "text": stringifyAny(m["text"])}, nil
	case "input_image", "image":
		if opts.OmitImages {
			return map[string]any{"type": "text", "text": historicalImageText}, nil
		}
		if !opts.SupportsImages {
			return map[string]any{"type": "text", "text": omittedImageText}, nil
		}
		source, _ := m["source"].(map[string]any)
		if source != nil && stringifyAny(source["type"]) == "base64" {
			data := stringifyAny(source["data"])
			if data == "" {
				data = stringifyAny(source["base64"])
			}
			if data != "" {
				mediaType := stringifyAny(source["media_type"])
				if mediaType == "" {
					mediaType = stringifyAny(source["mediaType"])
				}
				if mediaType == "" {
					mediaType = stringifyAny(m["media_type"])
				}
				return convertBase64Image(mediaType, data), nil
			}
		}
		imageURL := ""
		if iu, ok := m["image_url"].(map[string]any); ok {
			imageURL = stringifyAny(iu["url"])
		} else if s := stringifyAny(m["image_url"]); s != "" {
			imageURL = s
		}
		if imageURL == "" {
			imageURL = stringifyAny(m["url"])
		}
		if imageURL == "" && source != nil {
			imageURL = stringifyAny(source["url"])
		}
		if imageURL == "" {
			return nil, newAdapterError("input_image requires image_url")
		}
		mediaType, data, ok := parseDataUrlImage(imageURL)
		if !ok {
			return nil, newAdapterError("remote image URLs are not supported by the Messages adapter yet")
		}
		return convertBase64Image(mediaType, data), nil
	case "tool_result":
		toolUseID := stringifyAny(m["tool_use_id"])
		if toolUseID == "" {
			toolUseID = stringifyAny(m["call_id"])
		}
		if toolUseID == "" {
			toolUseID = stringifyAny(m["id"])
		}
		if toolUseID == "" {
			return nil, newAdapterError("tool_result requires tool_use_id or call_id")
		}
		rawContent := m["content"]
		if rawContent == nil {
			rawContent = m["output"]
		}
		// content 允许是字符串或内容块数组（Messages 协议两者都接受）。
		// 数组必须逐块转换后原样带上，不能整体字符串化 —— 否则会把结构体渲染成
		// 调试字面量塞给模型（原实现此处 String(array) 得到 "[object Object]"，同样是丢数据）。
		if arr, ok := rawContent.([]any); ok {
			blocks := make([]any, 0, len(arr))
			for _, b := range arr {
				converted, err := convertContentBlock(b, opts)
				if err != nil {
					return nil, err
				}
				if converted != nil {
					blocks = append(blocks, converted)
				}
			}
			return map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": blocks}, nil
		}
		return map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": stringifyAny(rawContent)}, nil
	}
	if isStateOnlyItem(m) {
		return nil, nil
	}
	t := stringifyAny(m["type"])
	if t == "" {
		t = "unknown"
	}
	return nil, newAdapterError("unsupported Responses content block type: %s", t)
}

// convertToolItem 转换工具类 input 项（function_call / custom_tool_call / *_output）。
// 返回 (nil, nil) 表示不是工具项，交给 convertMessageItem 继续处理。
func convertToolItem(item map[string]any, pairedToolCallIDs map[string]bool, opts adapterOptions) (map[string]any, error) {
	switch stringifyAny(item["type"]) {
	case "function_call":
		id := stringifyAny(item["call_id"])
		if id == "" {
			id = stringifyAny(item["id"])
		}
		if id == "" {
			return nil, newAdapterError("function_call requires call_id or id")
		}
		fn, _ := item["function"].(map[string]any)
		name := stringifyAny(item["name"])
		if name == "" && fn != nil {
			name = stringifyAny(fn["name"])
		}
		if name == "" {
			name = "tool"
		}
		args := item["arguments"]
		if args == nil {
			args = item["input"]
		}
		if args == nil && fn != nil {
			args = fn["arguments"]
		}
		return map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": parseToolInput(args),
		}}}, nil

	case "custom_tool_call":
		id := stringifyAny(item["call_id"])
		if id == "" {
			id = stringifyAny(item["id"])
		}
		if id == "" {
			return nil, newAdapterError("custom_tool_call requires call_id or id")
		}
		name := stringifyAny(item["name"])
		if name == "" {
			name = "custom_tool"
		}
		// 与请求侧 convertCustomToolDefinition 的单字符串参数对齐，才能和 tool_result 配对
		return map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": id, "name": name,
			"input": map[string]any{customToolInputKey: stringifyAny(item["input"])},
		}}}, nil

	case "function_call_output", "custom_tool_call_output", "tool_result":
		itemType := stringifyAny(item["type"])
		id := stringifyAny(item["call_id"])
		if id == "" {
			id = stringifyAny(item["tool_use_id"])
		}
		if id == "" {
			id = stringifyAny(item["id"])
		}
		if id == "" {
			return nil, newAdapterError("%s requires call_id or tool_use_id", itemType)
		}
		rawOutput := item["output"]
		if rawOutput == nil {
			rawOutput = item["content"]
		}
		if rawOutput == nil {
			rawOutput = ""
		}
		// output 可以是字符串或内容块数组
		var outputStr string
		var outputBlocks []any
		if arr, ok := rawOutput.([]any); ok {
			for _, b := range arr {
				converted, err := convertContentBlock(b, opts)
				if err != nil {
					return nil, err
				}
				if converted != nil {
					outputBlocks = append(outputBlocks, converted)
				}
			}
		} else {
			outputStr = stringifyAny(rawOutput)
		}
		if pairedToolCallIDs[id] {
			var content any = outputStr
			if outputBlocks != nil {
				content = outputBlocks
			}
			return map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": id, "content": content,
			}}}, nil
		}
		// 没有配对的 tool_use：降级为普通文本，避免上游报 orphan tool_result
		text := outputStr
		if outputBlocks != nil {
			text = jsonStringify(outputBlocks)
		}
		return map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "text", "text": "Tool result " + id + ":\n\n" + text,
		}}}, nil
	}
	return nil, nil
}

// convertMessageItem 转换单个 input 项为 Messages message。返回 (nil, nil) 表示跳过。
func convertMessageItem(item any, pairedToolCallIDs map[string]bool, opts adapterOptions) (map[string]any, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return nil, newAdapterError("unsupported Responses input item")
	}
	toolItem, err := convertToolItem(m, pairedToolCallIDs, opts)
	if err != nil {
		return nil, err
	}
	if toolItem != nil {
		return toolItem, nil
	}
	if isStateOnlyItem(m) {
		return nil, nil
	}
	role := normalizeRole(m["role"])
	rawContent := m["content"]
	if rawContent == nil {
		rawContent = m["text"]
	}
	if rawContent == nil {
		rawContent = m["output"]
	}
	content, err := asContentArray(rawContent)
	if err != nil {
		return nil, err
	}
	// assistant 文本里夹带旧版"Tool call call_xxx(...)"转录时逐块中性化，避免模型模仿该格式。
	// 与模型能力无关，只对确实命中该格式的文本块生效；其余块（含 tool_use）照常转换。
	if role == "assistant" {
		content = neutralizeAssistantTextBlocks(content)
	}
	var converted []any
	for _, block := range content {
		c, err := convertContentBlock(block, opts)
		if err != nil {
			return nil, err
		}
		if c != nil {
			converted = append(converted, c)
		}
	}
	if len(converted) == 0 {
		return nil, nil
	}
	return map[string]any{"role": role, "content": converted}, nil
}

// firstContentOfType 返回 message.content 里第一个指定 type 的块。
func firstContentOfType(message map[string]any, blockType string) map[string]any {
	content, ok := message["content"].([]any)
	if !ok {
		return nil
	}
	for _, block := range content {
		if bm, ok := block.(map[string]any); ok && stringifyAny(bm["type"]) == blockType {
			return bm
		}
	}
	return nil
}

// normalizeToolResultOrder 保证每个 tool_result 紧跟对应的 tool_use（Messages 协议要求配对顺序）。
func normalizeToolResultOrder(messages []map[string]any) []map[string]any {
	pendingResults := map[string]map[string]any{}
	var pendingOrder []string
	var withoutResults []map[string]any
	for _, message := range messages {
		toolResult := firstContentOfType(message, "tool_result")
		content, _ := message["content"].([]any)
		if toolResult != nil && len(content) == 1 {
			if id := stringifyAny(toolResult["tool_use_id"]); id != "" {
				pendingResults[id] = message
				pendingOrder = append(pendingOrder, id)
				continue
			}
		}
		withoutResults = append(withoutResults, message)
	}
	if len(pendingResults) == 0 {
		return messages
	}
	var ordered []map[string]any
	for _, message := range withoutResults {
		ordered = append(ordered, message)
		toolUse := firstContentOfType(message, "tool_use")
		if toolUse == nil {
			continue
		}
		id := stringifyAny(toolUse["id"])
		if id == "" {
			continue
		}
		if result, ok := pendingResults[id]; ok {
			ordered = append(ordered, result)
			delete(pendingResults, id)
		}
	}
	// 剩余未配对的按原顺序补在末尾
	for _, id := range pendingOrder {
		if result, ok := pendingResults[id]; ok {
			ordered = append(ordered, result)
			delete(pendingResults, id)
		}
	}
	return ordered
}

// inputToMessages 把 Responses 的 input 转成 Messages 的 messages。
func inputToMessages(input any, opts adapterOptions) ([]map[string]any, error) {
	if s, ok := input.(string); ok {
		return []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": s}}}}, nil
	}
	arr, ok := input.([]any)
	if !ok {
		return nil, newAdapterError("Responses input must be a string or array for Messages adapter")
	}
	// 找出"当前输入"下标（最后一个非状态、非 system 项）：只有它的图片保留
	currentInputIndex := len(arr) - 1
	for i := len(arr) - 1; i >= 0; i-- {
		m, _ := arr[i].(map[string]any)
		if !isStateOnlyItem(arr[i]) && (m == nil || stringifyAny(m["role"]) != "system") {
			currentInputIndex = i
			break
		}
	}
	pairedToolCallIDs := map[string]bool{}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// custom_tool_call 也要登记：它的 output 同样需要配成 tool_result，
		// 否则会被当成 orphan 降级为纯文本，工具回合就断了
		switch stringifyAny(m["type"]) {
		case "function_call", "custom_tool_call":
		default:
			continue
		}
		id := stringifyAny(m["call_id"])
		if id == "" {
			id = stringifyAny(m["id"])
		}
		if id != "" {
			pairedToolCallIDs[id] = true
		}
	}
	var messages []map[string]any
	for i, item := range arr {
		itemOpts := opts
		itemOpts.OmitImages = opts.OmitImages || i != currentInputIndex
		converted, err := convertMessageItem(item, pairedToolCallIDs, itemOpts)
		if err != nil {
			return nil, err
		}
		if converted != nil {
			messages = append(messages, converted)
		}
	}
	if len(messages) == 0 {
		return nil, newAdapterError("Responses input did not contain any Messages-compatible user or assistant content")
	}
	return normalizeToolResultOrder(messages), nil
}

// extractSystem 抽取 system 提示：instructions + input 里 role=system 的文本。
// 单条返回字符串，多条返回块数组（与 Messages 协议一致）；无返回 nil。
func extractSystem(doc map[string]any, opts adapterOptions) any {
	var system []map[string]any
	// instructions 可以是字符串或内容块数组（Responses 规范）；只看 string 会在数组形式时把
	// 整个系统提示丢掉，模型收不到系统指令却没有任何报错，极难排查。
	switch v := doc["instructions"].(type) {
	case string:
		if v != "" {
			system = append(system, map[string]any{"type": "text", "text": v})
		}
	case []any:
		for _, block := range v {
			converted, err := convertContentBlock(block, opts)
			if err != nil || converted == nil {
				continue
			}
			if stringifyAny(converted["type"]) == "text" {
				system = append(system, converted)
			}
		}
	}
	if input, ok := doc["input"].([]any); ok {
		for _, item := range input {
			m, ok := item.(map[string]any)
			if !ok || stringifyAny(m["role"]) != "system" {
				continue
			}
			content, err := asContentArray(m["content"])
			if err != nil {
				continue
			}
			for _, block := range content {
				// system 的图片能力取决于整个请求的 SupportsImages，用零值 adapterOptions{}
				// 会让支持图片的模型上 system 里的图片也被换成占位文本（内容错误+浪费 token）。
				converted, err := convertContentBlock(block, opts)
				if err != nil || converted == nil {
					continue
				}
				if stringifyAny(converted["type"]) == "text" {
					system = append(system, converted)
				}
			}
		}
	}
	if len(system) == 0 {
		return nil
	}
	if len(system) == 1 {
		return stringifyAny(system[0]["text"])
	}
	out := make([]any, 0, len(system))
	for _, s := range system {
		out = append(out, s)
	}
	return out
}

// responsesToMessagesRequest 把 Responses 请求体转成 Messages 请求体。
func responsesToMessagesRequest(doc map[string]any, opts adapterOptions) (map[string]any, error) {
	if doc == nil {
		return nil, newAdapterError("Responses request body must be JSON object")
	}
	messages, err := inputToMessages(doc["input"], opts)
	if err != nil {
		return nil, err
	}
	// system 已抽到顶层 system 字段，messages 里不再保留
	filtered := make([]any, 0, len(messages))
	for _, m := range messages {
		if stringifyAny(m["role"]) != "system" {
			filtered = append(filtered, m)
		}
	}
	out := map[string]any{"messages": filtered}
	if model := doc["model"]; model != nil {
		out["model"] = model
	}
	if system := extractSystem(doc, opts); system != nil {
		out["system"] = system
	}
	if stream, has := doc["stream"]; has {
		// Responses 允许布尔值或字符串形式（"true"/"false"）；只看 bool 会把 "true" 当成 false
		var streamBool bool
		switch v := stream.(type) {
		case bool:
			streamBool = v
		case string:
			streamBool = strings.EqualFold(v, "true")
		}
		out["stream"] = streamBool
	}
	maxTokens := defaultMessagesMaxTokens
	if v, ok := doc["max_output_tokens"].(float64); ok && v > 0 {
		// 0 或负数不透传：上游会 400，悄悄回退默认比让请求失败更有用
		maxTokens = int(v)
	}
	out["max_tokens"] = maxTokens
	for _, k := range []string{"temperature", "top_p"} {
		if v := doc[k]; v != nil {
			out[k] = v
		}
	}
	if v := doc["stop"]; v != nil {
		out["stop_sequences"] = v
	}
	if tools := convertTools(doc["tools"]); tools != nil {
		out["tools"] = tools
	}
	applyToolChoice(out, doc["tool_choice"])
	return out, nil
}

// applyToolChoice 把 Responses 的 tool_choice 映射到 Messages 的 tool_choice。
// "none" 不映射成 {"type":"none"}（该取值并非所有上游都认），而是直接去掉 tools —— 没有工具可用
// 等价于禁止调用，且不会因未知枚举被上游 400。其余取值静默忽略（保持原有宽松行为）。
func applyToolChoice(out map[string]any, choice any) {
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		case "required", "any":
			out["tool_choice"] = map[string]any{"type": "any"}
		case "none":
			delete(out, "tools")
		}
	case map[string]any:
		// {"type":"function","name":"x"} / {"type":"tool","name":"x"} → {"type":"tool","name":"x"}
		name := stringifyAny(v["name"])
		if name == "" {
			if fn, ok := v["function"].(map[string]any); ok {
				name = stringifyAny(fn["name"])
			}
		}
		switch stringifyAny(v["type"]) {
		case "function", "tool", "custom":
			if name != "" {
				out["tool_choice"] = map[string]any{"type": "tool", "name": name}
			}
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		case "any", "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		case "none":
			delete(out, "tools")
		}
	}
}

// convertOutputContent 把 Messages 的 content 转成 Responses 的 output_text 块。
func convertOutputContent(content any) []any {
	arr, ok := content.([]any)
	if !ok {
		return []any{}
	}
	out := []any{}
	for _, block := range arr {
		m, ok := block.(map[string]any)
		if !ok || stringifyAny(m["type"]) != "text" {
			continue
		}
		out = append(out, map[string]any{
			"type": "output_text", "text": sanitizeAssistantText(stringifyAny(m["text"])), "annotations": []any{},
		})
	}
	return out
}

// convertOutputItems 把 Messages 响应转成 Responses 的 output 数组。
// 规则：工具调用先输出为 function_call/custom_tool_call；若同时有文本，文本单独作为 message 项追加。
// 只有纯工具调用时不输出文本（避免空 message 块）；只有纯文本时只输出 message。
func convertOutputItems(doc map[string]any, nowMs int64, customTools map[string]bool) []any {
	content, _ := doc["content"].([]any)
	var toolCalls []any
	for _, block := range content {
		if m, ok := block.(map[string]any); ok && stringifyAny(m["type"]) == "tool_use" {
			toolCalls = append(toolCalls, toolUseToOutputItem(m, nowMs, customTools))
		}
	}
	// 同时有文本块：把文字内容包成 message 追加，让调用方既能看到工具调用也能看到推理文字
	textContent := convertOutputContent(doc["content"])
	if len(toolCalls) > 0 {
		if len(textContent) > 0 {
			id := stringifyAny(doc["id"])
			if id == "" {
				id = fmt.Sprintf("msg_%d", nowMs)
			}
			role := stringifyAny(doc["role"])
			if role == "" {
				role = "assistant"
			}
			msg := map[string]any{"id": id, "type": "message", "status": "completed", "role": role, "content": textContent}
			return append(toolCalls, msg)
		}
		return toolCalls
	}
	id := stringifyAny(doc["id"])
	if id == "" {
		id = fmt.Sprintf("msg_%d", nowMs)
	}
	role := stringifyAny(doc["role"])
	if role == "" {
		role = "assistant"
	}
	return []any{map[string]any{
		"id": id, "type": "message", "status": "completed", "role": role,
		"content": textContent,
	}}
}

// totalUsage 汇总 token 用量。
func totalUsage(usage any) map[string]any {
	m, _ := usage.(map[string]any)
	input, output := 0, 0
	if v, ok := m["input_tokens"].(float64); ok {
		input = int(v)
	}
	if v, ok := m["output_tokens"].(float64); ok {
		output = int(v)
	}
	return map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": input + output}
}

// messagesToResponsesBody 把 Messages 非流式响应转回 Responses 格式。
// customTools 是请求里声明为 freeform 的工具名，用于把对应 tool_use 还原成 custom_tool_call。
func messagesToResponsesBody(doc map[string]any, nowMs, createdAt int64, customTools map[string]bool) (map[string]any, error) {
	if doc == nil {
		return nil, newAdapterError("Messages response body must be JSON object")
	}
	id := stringifyAny(doc["id"])
	if id == "" {
		id = fmt.Sprintf("msg_%d", nowMs)
	}
	responseID := id
	if !strings.HasPrefix(responseID, "resp_") {
		responseID = "resp_" + responseID
	}
	return map[string]any{
		"id": responseID, "object": "response", "created_at": createdAt,
		// stop_reason=max_tokens 也算完成：Messages 侧仍可能带回有用文本，
		// 而 Codex 把 response.incomplete 当失败轮次处理
		"status": "completed", "model": doc["model"],
		"output": convertOutputItems(doc, nowMs, customTools), "usage": totalUsage(doc["usage"]),
	}, nil
}

// sortedContentIndexes 返回升序的内容块下标（SSE 收尾拼接文本用）。
func sortedContentIndexes(m map[int]string) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
