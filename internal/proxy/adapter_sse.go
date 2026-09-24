package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Messages SSE → Responses SSE 增量转换。
// 有状态：只处理到最后一个完整事件边界，剩余留在 pending 等下一个 chunk。

// sseFrameParsed 是解析出的一个 SSE 事件。
type sseFrameParsed struct {
	event string
	data  string
}

var sseEventLinePattern = regexp.MustCompile(`(?i)^event:\s*(.*)$`)
var sseDataLinePattern = regexp.MustCompile(`(?i)^data:\s?(.*)$`)

// parseSSEFrames 解析 SSE 文本为事件列表（空行分隔，data 多行拼接）。
func parseSSEFrames(text string) []sseFrameParsed {
	var frames []sseFrameParsed
	event := "message"
	var dataLines []string
	for _, line := range splitLines(text) {
		if strings.TrimSpace(line) == "" {
			if len(dataLines) > 0 {
				frames = append(frames, sseFrameParsed{event: event, data: strings.Join(dataLines, "\n")})
			}
			event = "message"
			dataLines = nil
			continue
		}
		if m := sseEventLinePattern.FindStringSubmatch(line); m != nil {
			event = strings.TrimSpace(m[1])
			if event == "" {
				event = "message"
			}
			continue
		}
		if m := sseDataLinePattern.FindStringSubmatch(line); m != nil {
			dataLines = append(dataLines, m[1])
		}
	}
	return frames
}

// sseFrame 格式化一个 Responses SSE 事件帧。
func sseFrame(event string, data any) string {
	return "event: " + event + "\ndata: " + jsonStringify(data) + "\n\n"
}

// toolUseState 是一个 tool_use 内容块的累积状态。
type toolUseState struct {
	block       map[string]any
	partialJSON string
	item        map[string]any
	custom      bool // freeform 工具：收尾发 custom_tool_call_input.done，不发 arguments.delta
}

// messagesSSEState 是转换器的跨 chunk 状态。
type messagesSSEState struct {
	responseID          string
	messageID           string
	model               any
	createdAt           int64
	messageItemAdded    bool
	messageOutputIndex  int
	nextOutputIndex     int
	nextContentIndex    int
	inputTokens         int
	outputTokens        int
	contentTextByIndex  map[int]string
	rawConsumedByIndex  map[int]int    // 已处理/下发的原文字节数（只扫未处理尾部，避免逐帧全量重扫的 O(n²)）
	contentIndexByBlock map[int]int    // block index -> 已分配的 content_index
	outputIndexByBlock  map[int]int    // block index -> 已分配的 output_index
	blockTypeByIndex    map[int]string // block index -> 上游块类型（text/tool_use/thinking/...）
	toolUseByIndex      map[int]*toolUseState
	customTools         map[string]bool
	done                bool // 已发出终态（failed 或 completed），后续帧全部丢弃
}

// MessagesSSETransformer 是有状态的增量转换器。
type MessagesSSETransformer struct {
	pending string
	state   *messagesSSEState
}

// newMessagesSSETransformer 构造转换器。model 用于回填 response.model；
// customTools 是请求里声明为 freeform 的工具名，用于把 tool_use 还原成 custom_tool_call。
func newMessagesSSETransformer(model string, customTools map[string]bool) *MessagesSSETransformer {
	nowMs := time.Now().UnixMilli()
	return &MessagesSSETransformer{state: &messagesSSEState{
		responseID:          fmt.Sprintf("resp_%d", nowMs),
		messageID:           "msg_0",
		model:               model,
		createdAt:           nowMs / 1000, // Responses 协议 created_at 是 Unix 秒（与 Chat SSE/非流式一致）
		messageOutputIndex:  -1,
		contentTextByIndex:  map[int]string{},
		rawConsumedByIndex:  map[int]int{},
		contentIndexByBlock: map[int]int{},
		outputIndexByBlock:  map[int]int{},
		blockTypeByIndex:    map[int]string{},
		toolUseByIndex:      map[int]*toolUseState{},
		customTools:         customTools,
	}}
}

func (st *messagesSSEState) outputIndexForBlock(idx int) int {
	if output, ok := st.outputIndexByBlock[idx]; ok {
		return output
	}
	output := st.nextOutputIndex
	st.nextOutputIndex++
	st.outputIndexByBlock[idx] = output
	return output
}

func (st *messagesSSEState) ensureMessageOutputIndex() int {
	if st.messageOutputIndex < 0 {
		st.messageOutputIndex = st.nextOutputIndex
		st.nextOutputIndex++
	}
	return st.messageOutputIndex
}

func (st *messagesSSEState) contentIndexForBlock(idx int) int {
	if content, ok := st.contentIndexByBlock[idx]; ok {
		return content
	}
	content := st.nextContentIndex
	st.nextContentIndex++
	st.contentIndexByBlock[idx] = content
	return content
}

// mergeUsage 累积 Messages 的 usage（input_tokens 在 message_start，output_tokens 在 message_delta）。
// 取最大值而非累加：同一字段可能在多个事件里重复给出当前累计值。
func (st *messagesSSEState) mergeUsage(usage any) {
	m, ok := usage.(map[string]any)
	if !ok {
		return
	}
	if v, ok := m["input_tokens"].(float64); ok && int(v) > st.inputTokens {
		st.inputTokens = int(v)
	}
	if v, ok := m["output_tokens"].(float64); ok && int(v) > st.outputTokens {
		st.outputTokens = int(v)
	}
}

// lastSSEBoundary 返回最后一个完整事件边界的结束位置，找不到返回 -1。
func lastSSEBoundary(s string) int {
	end := -1
	if i := strings.LastIndex(s, "\n\n"); i >= 0 {
		end = i + 2
	}
	if i := strings.LastIndex(s, "\r\n\r\n"); i >= 0 && i+4 > end {
		end = i + 4
	}
	return end
}

// Push 处理一个 chunk，返回可立即写给客户端的 Responses SSE 文本（可能为空）。
func (t *MessagesSSETransformer) Push(chunk string) string {
	combined := t.pending + chunk
	end := lastSSEBoundary(combined)
	if end < 0 {
		t.pending = combined
		return ""
	}
	complete := combined[:end]
	t.pending = combined[end:]
	return convertMessagesSSE(complete, t.state)
}

// Flush 收尾：处理残留 pending。
func (t *MessagesSSETransformer) Flush() string {
	if t.pending == "" {
		return ""
	}
	out := convertMessagesSSE(t.pending+"\n\n", t.state)
	t.pending = ""
	return out
}

// Usage 返回累积的 input/output token 数（计量用；Flush 后读取）。
func (t *MessagesSSETransformer) Usage() (in, out int64) {
	return int64(t.state.inputTokens), int64(t.state.outputTokens)
}

// intIndex 从事件的 index 字段取整数下标（缺省 0）。
func intIndex(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// convertMessagesSSE 把一段完整的 Messages SSE 转成 Responses SSE（对应 messagesSseToResponsesSse）。
func convertMessagesSSE(text string, st *messagesSSEState) string {
	var out strings.Builder
	for _, frame := range parseSSEFrames(text) {
		// 已进终态（response.failed 或 response.completed 之后）就不再产出任何帧：
		// 上游在 error 之后仍发 message_stop 时，会先 failed 再 completed + 第二个 [DONE]，
		// 客户端拿到自相矛盾的状态。
		if st.done {
			continue
		}
		if strings.TrimSpace(frame.data) == "[DONE]" {
			out.WriteString("data: [DONE]\n\n")
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(frame.data), &parsed); err != nil {
			continue
		}
		eventType := stringifyAny(parsed["type"])
		if eventType == "" {
			eventType = frame.event
		}
		switch {
		case eventType == "message_start":
			message, _ := parsed["message"].(map[string]any)
			if id := stringifyAny(message["id"]); id != "" {
				st.messageID = id
			}
			if m := message["model"]; m != nil {
				st.model = m
			}
			// Messages 把 input_tokens 放在 message_start、output_tokens 放在 message_delta，
			// 累积后在 response.completed 里带出去，否则客户端的 token 计数恒为 0。
			st.mergeUsage(message["usage"])
			out.WriteString(sseFrame("response.created", map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id": st.responseID, "object": "response", "status": "in_progress", "model": st.model,
					"created_at": st.createdAt,
				},
			}))

		case eventType == "content_block_start":
			block, _ := parsed["content_block"].(map[string]any)
			idx := intIndex(parsed["index"])
			blockType := stringifyAny(block["type"])
			st.blockTypeByIndex[idx] = blockType
			switch blockType {
			case "text":
				outputIndex := st.ensureMessageOutputIndex()
				contentIndex := st.contentIndexForBlock(idx)
				// 存原文（不在此处清洗）：流式下发与收尾都基于累计原文统一做跨帧安全清洗。
				st.contentTextByIndex[idx] = stringifyAny(block["text"])
				if !st.messageItemAdded {
					st.messageItemAdded = true
					out.WriteString(sseFrame("response.output_item.added", map[string]any{
						"type": "response.output_item.added", "output_index": outputIndex,
						"item": map[string]any{"id": st.messageID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
					}))
				}
				out.WriteString(sseFrame("response.content_part.added", map[string]any{
					"type": "response.content_part.added", "item_id": st.messageID,
					"output_index": outputIndex, "content_index": contentIndex,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				}))
			case "tool_use":
				outputIndex := st.outputIndexForBlock(idx)
				// 先以空入参生成骨架，实际入参靠 input_json_delta 累积。
				// 首帧状态用 in_progress + 空入参（与 Chat SSE 路径一致），
				// 真实入参与 completed 状态在 content_block_stop 收尾时给出，
				// 避免 codex 按 .added 派发到"已完成且入参为 {}"的空调用。
				skeleton := map[string]any{}
				for k, v := range block {
					skeleton[k] = v
				}
				skeleton["input"] = map[string]any{}
				item := toolUseToOutputItem(skeleton, time.Now().UnixMilli(), st.customTools)
				item["status"] = "in_progress"
				if stringifyAny(item["type"]) == "custom_tool_call" {
					item["input"] = ""
				} else {
					item["arguments"] = ""
				}
				custom := stringifyAny(item["type"]) == "custom_tool_call"
				st.toolUseByIndex[idx] = &toolUseState{block: block, item: item, custom: custom}
				out.WriteString(sseFrame("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": outputIndex, "item": item,
				}))
			}

		case eventType == "content_block_delta":
			delta, _ := parsed["delta"].(map[string]any)
			idx := intIndex(parsed["index"])
			switch stringifyAny(delta["type"]) {
			case "text_delta":
				// 只对 text（或未声明类型的——部分上游如 minimax 直接发 text_delta
				// 无 content_block_start）块累积正文；明确是 thinking/image 等非 text
				// 块即便上游（非规范地）发 text_delta 也不下发（无 content_part.added
				// 配对，孤儿 delta）。
				switch st.blockTypeByIndex[idx] {
				case "text", "":
				default:
					continue
				}
				outputIndex := st.ensureMessageOutputIndex()
				contentIndex := st.contentIndexForBlock(idx)
				raw := stringifyAny(delta["text"])
				st.contentTextByIndex[idx] += raw
				// 只对"未处理尾部"做跨帧安全切分与清洗（已处理前缀是稳定安全的，无需重扫）：
				// 把 <think>/minimax 标记与未闭合 think 区段留在尾部缓冲，避免原始标记泄漏；
				// 且避免逐帧对整段累积文本重跑正则的 O(n²)。
				consumed := st.rawConsumedByIndex[idx]
				pending := st.contentTextByIndex[idx][consumed:]
				if safe, _ := streamSafeSplit(pending); safe != "" {
					st.rawConsumedByIndex[idx] = consumed + len(safe)
					if chunk := sanitizeAssistantText(safe); chunk != "" {
						out.WriteString(sseFrame("response.output_text.delta", map[string]any{
							"type": "response.output_text.delta", "item_id": st.messageID,
							"output_index": outputIndex, "content_index": contentIndex, "delta": chunk,
						}))
					}
				}
			case "input_json_delta":
				toolUse := st.toolUseByIndex[idx]
				if toolUse == nil {
					continue
				}
				partial := stringifyAny(delta["partial_json"])
				toolUse.partialJSON += partial
				// freeform 工具的入参是 {"input":"..."} 包装，逐块转发会把 JSON 骨架
				// 泄漏给客户端；拆包只能等收全，所以增量阶段不发事件，收尾一次性发 input.done
				if toolUse.custom {
					continue
				}
				out.WriteString(sseFrame("response.function_call_arguments.delta", map[string]any{
					"type":    "response.function_call_arguments.delta",
					"item_id": toolUse.item["id"], "output_index": st.outputIndexForBlock(idx), "delta": partial,
				}))
			}

		case eventType == "content_block_stop":
			idx := intIndex(parsed["index"])
			if toolUse := st.toolUseByIndex[idx]; toolUse != nil {
				outputIndex := st.outputIndexForBlock(idx)
				argumentsText := toolUse.partialJSON
				if argumentsText == "" {
					argumentsText = stableJson(toolUse.block["input"])
				}
				item := map[string]any{}
				for k, v := range toolUse.item {
					item[k] = v
				}
				item["status"] = "completed"
				if toolUse.custom {
					// custom_tool_call 的载荷是顶层 input 纯文本，不是 arguments JSON 串
					input := customToolInputText(parseToolInput(argumentsText))
					item["input"] = input
					toolUse.item = item
					out.WriteString(sseFrame("response.custom_tool_call_input.done", map[string]any{
						"type":    "response.custom_tool_call_input.done",
						"item_id": item["id"], "output_index": outputIndex, "input": input,
					}))
				} else {
					item["arguments"] = argumentsText
					toolUse.item = item
					out.WriteString(sseFrame("response.function_call_arguments.done", map[string]any{
						"type":    "response.function_call_arguments.done",
						"item_id": item["id"], "output_index": outputIndex, "arguments": argumentsText,
					}))
				}
				out.WriteString(sseFrame("response.output_item.done", map[string]any{
					"type": "response.output_item.done", "output_index": outputIndex, "item": item,
				}))
				continue
			}
			// 非 tool_use 块：只有 text 块才发 content_part.done 收尾。
			// thinking/redacted_thinking/image 等块在 content_block_start/delta 从未被
			// 下发（正文直接丢弃、无 content_part.added），这里若照文本分支处理会给
			// 客户端一个没有 .added 配对的孤儿 content_part.done，且 content_index 错位。
			if st.blockTypeByIndex[idx] != "text" {
				continue
			}
			contentIndex := st.contentIndexForBlock(idx)
			// 收尾：把未处理尾部按收尾规则清洗后补发为最后一段 delta（保证增量流完整），
			// 再发 content_part.done。finalizeSanitize 会丢弃未闭合 think 区段。
			full := st.contentTextByIndex[idx]
			if tail := finalizeSanitize(full[st.rawConsumedByIndex[idx]:]); tail != "" {
				out.WriteString(sseFrame("response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": st.messageID,
					"output_index": st.ensureMessageOutputIndex(), "content_index": contentIndex, "delta": tail,
				}))
			}
			st.rawConsumedByIndex[idx] = len(full)
			out.WriteString(sseFrame("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "item_id": st.messageID,
				"output_index": st.ensureMessageOutputIndex(), "content_index": contentIndex,
				"part": map[string]any{"type": "output_text", "text": finalizeSanitize(full), "annotations": []any{}},
			}))

		case eventType == "message_delta":
			st.mergeUsage(parsed["usage"])

		case eventType == "error":
			var errPayload any = parsed["error"]
			if errPayload == nil {
				errPayload = parsed
			}
			out.WriteString(sseFrame("response.failed", map[string]any{
				"type": "response.failed",
				"response": map[string]any{"id": st.responseID, "object": "response", "status": "failed",
					"created_at": st.createdAt, "error": errPayload},
			}))
			out.WriteString("data: [DONE]\n\n")
			st.done = true // 终态：后续 message_stop 不能再翻成 completed

		case eventType == "message_stop":
			content := []any{}
			for _, idx := range sortedContentIndexes(st.contentTextByIndex) {
				content = append(content, map[string]any{
					"type": "output_text", "text": finalizeSanitize(st.contentTextByIndex[idx]), "annotations": []any{},
				})
			}
			if len(content) > 0 || len(st.toolUseByIndex) == 0 {
				out.WriteString(sseFrame("response.output_item.done", map[string]any{
					"type": "response.output_item.done", "output_index": st.ensureMessageOutputIndex(),
					"item": map[string]any{"id": st.messageID, "type": "message", "status": "completed", "role": "assistant", "content": content},
				}))
			}
			// stop_reason=max_tokens 也按 completed 收尾（Codex 把 incomplete 当失败轮次）
			response := map[string]any{"id": st.responseID, "object": "response", "status": "completed", "model": st.model,
				"created_at": st.createdAt}
			if st.inputTokens > 0 || st.outputTokens > 0 {
				response["usage"] = map[string]any{
					"input_tokens": st.inputTokens, "output_tokens": st.outputTokens,
					"total_tokens": st.inputTokens + st.outputTokens,
				}
			}
			out.WriteString(sseFrame("response.completed", map[string]any{
				"type": "response.completed", "response": response,
			}))
			out.WriteString("data: [DONE]\n\n")
			st.done = true // 终态：重复的 message_stop 不再产出第二份 completed/[DONE]
		}
	}
	return out.String()
}
