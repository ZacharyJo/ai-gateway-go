package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// bridge_imagegen 工具桥接（对齐 ai-gateway 的 bridge_imagegen 思路）。
// 默认关（BRIDGE_IMAGEGEN_ENABLED）：开启后向透传（GPT 系）请求注入 bridge_imagegen
// 工具与指令，模型调用它时代理转调上游图片 API（/images/generations 或 /images/edits），
// 把生成的图片保存到本地并把结果写回响应，替代 Codex 原生 image_generation 工具——
// 让不支持原生图片工具的模型也能生成图片。
//
// 图片落盘目录：<LogDir>/generated-images/，按请求时间+序号命名。

// bridgeImagegenToolName 是注入的工具名。
const bridgeImagegenToolName = "bridge_imagegen"

// imageAPITimeout 是上游图片 API 单次调用超时（生成通常 10-60s，给足余量）。
const imageAPITimeout = 120 * time.Second

// bridgeImagegenInstructions 是注入到请求里的工具使用指令。
const bridgeImagegenInstructions = "For requests to generate, redraw, replace, vary, or edit an image, " +
	"call the bridge_imagegen function. Use the conversation context and the user's feedback to write a " +
	"complete production-quality image prompt. Do not use shell commands, CLI scripts, or filesystem " +
	"searches to generate or locate substitute images. Do not reuse an image from another task. " +
	"For questions or troubleshooting about image generation, answer normally without calling the function."

// bridgeImagegenToolSchema 是 bridge_imagegen 的参数 schema（Responses 与 Messages 共用）。
func bridgeImagegenToolSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":            map[string]any{"type": "string", "enum": []any{"generate", "edit"}},
			"prompt":            map[string]any{"type": "string"},
			"source_image_path": map[string]any{"type": "string", "description": "absolute path; required for action=edit"},
			"size":              map[string]any{"type": "string"},
			"quality":           map[string]any{"type": "string"},
			"output_format":     map[string]any{"type": "string"},
		},
		"required": []any{"prompt"},
	}
}

// bridgeImagegenToolDescription 是工具描述（各协议共用）。
const bridgeImagegenToolDescription = "Generate or edit a raster image through the proxy image bridge. " +
	"Pass a complete production-quality prompt; for edits pass an existing absolute source_image_path."

// injectBridgeImagegenTool 向 Responses 请求注入 bridge_imagegen 工具与指令。
// 幂等：已存在同名工具时跳过。返回是否修改了 doc。
func injectBridgeImagegenTool(doc map[string]any, cfg *Config) bool {
	tools, _ := doc["tools"].([]any)
	for _, t := range tools {
		if m, ok := t.(map[string]any); ok && stringifyAny(m["name"]) == bridgeImagegenToolName {
			return false
		}
	}
	tool := map[string]any{
		"type":        "function",
		"name":        bridgeImagegenToolName,
		"description": bridgeImagegenToolDescription,
		"parameters":  bridgeImagegenToolSchema(),
	}
	doc["tools"] = append(tools, tool)
	// 指令放进 system/developer 指令串，避免污染用户内容
	injectToolInstruction(doc)
	return true
}

// injectBridgeImagegenMessages 向 Messages 适配后的 body 注入 bridge_imagegen 工具（Messages 格式）。
func injectBridgeImagegenMessages(body []byte) ([]byte, bool) {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body, false
	}
	tools, _ := doc["tools"].([]any)
	for _, t := range tools {
		if m, ok := t.(map[string]any); ok && stringifyAny(m["name"]) == bridgeImagegenToolName {
			return body, false
		}
	}
	doc["tools"] = append(tools, map[string]any{
		"type":         "function",
		"name":         bridgeImagegenToolName,
		"description":  bridgeImagegenToolDescription,
		"input_schema": bridgeImagegenToolSchema(),
	})
	appendSystemInstructions(doc)
	b, err := json.Marshal(doc)
	if err != nil {
		return body, false
	}
	return b, true
}

// appendSystemInstructions 把桥接指令并入 Messages 的 system 字段。
// Anthropic 允许 system 为字符串或内容块数组：两种形态都处理，不覆盖原有内容。
func appendSystemInstructions(doc map[string]any) {
	switch sys := doc["system"].(type) {
	case string:
		if !strings.Contains(sys, bridgeImagegenToolName) {
			doc["system"] = sys + "\n\n" + bridgeImagegenInstructions
		}
	case []any:
		if !blocksContainInstruction(sys, "input_text") {
			doc["system"] = append(sys, map[string]any{"type": "input_text", "text": bridgeImagegenInstructions})
		}
	default:
		doc["system"] = bridgeImagegenInstructions
	}
}

// blocksContainInstruction 判断内容块数组里是否已含桥接指令（幂等）。
func blocksContainInstruction(blocks []any, textType string) bool {
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && stringifyAny(m["type"]) == textType {
			if strings.Contains(stringifyAny(m["text"]), bridgeImagegenToolName) {
				return true
			}
		}
	}
	return false
}

// injectBridgeImagegenChat 向 Chat Completions 适配后的 body 注入 bridge_imagegen 工具（OpenAI 格式）。
func injectBridgeImagegenChat(body []byte) ([]byte, bool) {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body, false
	}
	tools, _ := doc["tools"].([]any)
	for _, t := range tools {
		if m, ok := t.(map[string]any); ok {
			if fn, ok := m["function"].(map[string]any); ok && stringifyAny(fn["name"]) == bridgeImagegenToolName {
				return body, false
			}
		}
	}
	doc["tools"] = append(tools, map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        bridgeImagegenToolName,
			"description": bridgeImagegenToolDescription,
			"parameters":  bridgeImagegenToolSchema(),
		},
	})
	if messages, ok := doc["messages"].([]any); ok && len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok && stringifyAny(first["role"]) == "system" {
			// content 支持字符串或内容块数组：都不覆盖原有内容
			switch c := first["content"].(type) {
			case string:
				if !strings.Contains(c, bridgeImagegenToolName) {
					first["content"] = c + "\n\n" + bridgeImagegenInstructions
				}
			case []any:
				if !blocksContainInstruction(c, "text") {
					first["content"] = append(c, map[string]any{"type": "text", "text": bridgeImagegenInstructions})
				}
			default:
				first["content"] = bridgeImagegenInstructions
			}
			b, err := json.Marshal(doc)
			return b, err == nil
		}
		systemMsg := map[string]any{"role": "system", "content": bridgeImagegenInstructions}
		doc["messages"] = append([]any{systemMsg}, messages...)
	} else {
		// messages 为空/缺失：补一条 system 指令消息，模型才知道要用该工具
		systemMsg := map[string]any{"role": "system", "content": bridgeImagegenInstructions}
		doc["messages"] = []any{systemMsg}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return body, false
	}
	return b, true
}

// injectToolInstruction 把桥接指令追加到 developer/system 消息里（没有就补一条）。
func injectToolInstruction(doc map[string]any) {
	for _, key := range []string{"instructions", "system", "developer"} {
		if v, ok := doc[key].(string); ok {
			if !strings.Contains(v, bridgeImagegenToolName) {
				doc[key] = v + "\n\n" + bridgeImagegenInstructions
			}
			return
		}
	}
	// 无指令字段：把指令并进首条 developer 消息（没有再补一条到 input 开头）
	if input, ok := doc["input"].([]any); ok && len(input) > 0 {
		if first, ok := input[0].(map[string]any); ok && stringifyAny(first["type"]) == "message" && stringifyAny(first["role"]) == "developer" {
			if content, ok := first["content"].([]any); ok && len(content) > 0 {
				if cm, ok := content[0].(map[string]any); ok && stringifyAny(cm["type"]) == "input_text" {
					text := stringifyAny(cm["text"])
					if !strings.Contains(text, bridgeImagegenToolName) {
						cm["text"] = text + "\n\n" + bridgeImagegenInstructions
					}
					return
				}
			}
		}
	}
	dev := map[string]any{
		"type":    "message",
		"role":    "developer",
		"content": []any{map[string]any{"type": "input_text", "text": bridgeImagegenInstructions}},
	}
	existing, _ := doc["input"].([]any)
	doc["input"] = append([]any{dev}, existing...)
}

// imageBridgeArgs 是 bridge_imagegen 的参数。
type imageBridgeArgs struct {
	Action          string `json:"action"`
	Prompt          string `json:"prompt"`
	SourceImagePath string `json:"source_image_path"`
	Size            string `json:"size"`
	Quality         string `json:"quality"`
	OutputFormat    string `json:"output_format"`
}

// parseImageBridgeArgs 解析模型产出的工具参数（容忍 ```json 围栏、截断 JSON 里提取 prompt）。
func parseImageBridgeArgs(argumentsText string) *imageBridgeArgs {
	raw := strings.TrimSpace(argumentsText)
	if raw == "" {
		return nil
	}
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(raw, "```"), "```json"))
	candidates := []string{raw}
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			candidates = append(candidates, raw[i:j+1])
		}
	}
	for _, c := range candidates {
		var args imageBridgeArgs
		if err := json.Unmarshal([]byte(c), &args); err == nil && args.Prompt != "" {
			args.Action = strings.ToLower(strings.TrimSpace(args.Action))
			if args.Action != "edit" {
				args.Action = "generate"
			}
			return &args
		}
	}
	// 截断 JSON：用正则兜底提取 prompt
	promptMatch := regexp.MustCompile(`["']prompt["']\s*:\s*["']([\s\S]*?)["']\s*(?:,|})`).FindStringSubmatch(raw)
	if len(promptMatch) > 1 {
		prompt := strings.ReplaceAll(promptMatch[1], `\"`, `"`)
		prompt = strings.ReplaceAll(prompt, "\\n", "\n")
		action := "generate"
		if regexp.MustCompile(`["']action["']\s*:\s*["']edit["']`).MatchString(raw) {
			action = "edit"
		}
		return &imageBridgeArgs{Action: action, Prompt: prompt}
	}
	return nil
}

// imageBridgeResult 是一次图片桥接的执行结果。
type imageBridgeResult struct {
	b64JSON       string
	revisedPrompt string
	imagePath     string
}

// executeImageBridge 执行一次 bridge_imagegen 调用：转调上游图片 API，落盘，返回结果。
func (u *Upstream) executeImageBridge(args *imageBridgeArgs, r *http.Request, reqID int64) (*imageBridgeResult, error) {
	payload := map[string]any{
		"model":         u.cfg.ImageModel,
		"prompt":        args.Prompt,
		"size":          orString(args.Size, u.cfg.ImageSize),
		"quality":       orString(args.Quality, u.cfg.ImageQuality),
		"output_format": orString(args.OutputFormat, u.cfg.ImageOutputFormat),
		"n":             1,
	}
	endpoint := "/images/generations"
	contentType := "application/json"
	body := mustJSON(payload)
	if args.Action == "edit" {
		if args.SourceImagePath == "" {
			return nil, fmt.Errorf("bridge_imagegen edit requires source_image_path")
		}
		sourcePath := args.SourceImagePath
		if !strings.HasPrefix(sourcePath, "/") {
			sourcePath = filepath.Join(u.cfg.LogDir, sourcePath)
		}
		// 沙箱：只允许编辑本代理生成/管理的图片（<LogDir>/generated-images/ 下），
		// 防止提示注入诱导模型把宿主任意文件 base64 上传到上游编辑 API。
		if !underDir(sourcePath, filepath.Join(u.cfg.LogDir, "generated-images")) {
			return nil, fmt.Errorf("bridge_imagegen edit source must be under <log_dir>/generated-images: %s", sourcePath)
		}
		mediaType, data, err := readImageFile(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("bridge_imagegen edit cannot read %s: %v", sourcePath, err)
		}
		endpoint = "/images/edits"
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for k, v := range payload {
			if k == "n" {
				continue
			}
			_ = w.WriteField(k, fmt.Sprint(v))
		}
		_ = w.WriteField("n", "1")
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename="%s"`, filepath.Base(sourcePath)))
		h.Set("Content-Type", mediaType)
		part, _ := w.CreatePart(h)
		_, _ = part.Write(data)
		_ = w.Close()
		body = buf.Bytes()
		contentType = w.FormDataContentType()
	}

	resp, err := u.doImageAPI(r.Context(), endpoint, contentType, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 先判状态码再解 JSON：4xx/5xx 且非 JSON body 时报告真实状态码，而不是误报 invalid JSON。
	if resp.StatusCode >= 400 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return nil, fmt.Errorf("image API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(eb)))
	}
	var doc struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("image API returned invalid JSON: %v", err)
	}
	if len(doc.Data) == 0 || doc.Data[0].B64JSON == "" {
		return nil, fmt.Errorf("image API returned HTTP %d without data[0].b64_json", resp.StatusCode)
	}
	imagePath, err := u.persistBridgeImage(doc.Data[0].B64JSON, reqID)
	if err != nil {
		return nil, err
	}
	return &imageBridgeResult{
		b64JSON:       doc.Data[0].B64JSON,
		revisedPrompt: doc.Data[0].RevisedPrompt,
		imagePath:     imagePath,
	}, nil
}

// doImageAPI 发送一次上游图片 API 请求。
// 图片生成通常 10-60s：加显式超时兜底，避免上游挂起时无限阻塞 SSE 流。
// 认证与主转发路径一致：none 不注入任何认证头；bearer 注入 APIKey。
func (u *Upstream) doImageAPI(ctx context.Context, endpoint, contentType string, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, imageAPITimeout)
	defer cancel()
	target := *u.base
	target.Path = strings.TrimSuffix(u.base.Path, "/") + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if u.cfg.AuthMode == "bearer" && u.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.cfg.APIKey)
	}
	req.ContentLength = int64(len(body))
	return u.client.Do(req)
}

// readImageFile 读取图片文件，返回 (mediaType, bytes)。
func readImageFile(path string) (string, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	mediaType := "image/png"
	switch ext {
	case ".jpg", ".jpeg":
		mediaType = "image/jpeg"
	case ".webp":
		mediaType = "image/webp"
	case ".gif":
		mediaType = "image/gif"
	}
	return mediaType, data, nil
}

// underDir 判断 path 是否位于 dir 目录内（词法级：Abs + Rel，拒绝 .. 逃逸）。
func underDir(path, dir string) bool {
	absPath, err1 := filepath.Abs(path)
	absDir, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// persistBridgeImage 把图片 base64 落盘到 <LogDir>/generated-images/，返回绝对路径。
// 扩展名按 ImageOutputFormat（png/jpeg/webp）取，与文件实际内容一致。
func (u *Upstream) persistBridgeImage(b64 string, reqID int64) (string, error) {
	raw := decodeBase64(b64)
	if len(raw) == 0 {
		return "", fmt.Errorf("image API returned un-decodable b64_json")
	}
	dir := filepath.Join(u.cfg.LogDir, "generated-images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ext := strings.ToLower(strings.TrimSpace(u.cfg.ImageOutputFormat))
	// 白名单：配置的格式来自 ImageOutputFormat（png/jpeg/webp），防御性拒绝其它值
	//（含路径分隔符，避免 filepath.Join 逃逸 generated-images 目录）。
	switch ext {
	case "jpeg", "webp", "png":
	default:
		ext = "png"
	}
	name := fmt.Sprintf("image_%d_%d_%s.%s", time.Now().Unix(), reqID, randomSuffix(), ext)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// bridgeCallItem 是响应里一次 bridge_imagegen 调用（SSE 跟踪用）。
type bridgeCallItem struct {
	id            string
	outputIndex   int
	argumentsText string
}

// imageBridgeSSETransformer 是 SSE 路径的 bridge_imagegen 执行器。
// 跟踪 function_call(bridge_imagegen) 项，arguments 收尾时转调上游图片 API，
// 把结果以 image_generation_call + function_call_output 的形式发回。
// 内嵌 toolShapeRepairer：bridge 开启时 custom 工具降级修理仍然生效。
type imageBridgeSSETransformer struct {
	u        *Upstream
	r        *http.Request
	reqID    int64
	repairer *toolShapeRepairer
	tracked  map[string]*bridgeCallItem
	pending  string
	executed map[string]bool // 已执行的调用 id（output_item.done 不再重复执行）
	// results 记录每个已执行 bridge 调用的合成输出项（image_generation_call +
	// function_call_output），供 response.completed 帧合并，避免 completed 权威重建时丢结果。
	results map[string][]any
	// syntheticIdx 是桥接注入项的独立 output_index 分配器。从高基（1<<20）开始递增，
	// 与上游的小序号 output_index 永不冲突——即便上游在桥接工具之后继续发项。
	syntheticIdx int
}

// allocSynthetic 分配下一个桥接注入项的 output_index（高基，不与上游冲突）。
func (t *imageBridgeSSETransformer) allocSynthetic() int {
	idx := t.syntheticIdx
	t.syntheticIdx++
	return idx
}

// syntheticIndexBase 是桥接注入项 output_index 的起始基（远离上游的小序号区间）。
const syntheticIndexBase = 1 << 20

// newImageBridgeSSETransformer 构造 SSE 桥接执行器。
func newImageBridgeSSETransformer(u *Upstream, r *http.Request, reqID int64, repairTools map[string]bool) *imageBridgeSSETransformer {
	var repairer *toolShapeRepairer
	if len(repairTools) > 0 {
		repairer = newToolShapeRepairer(repairTools)
	}
	return &imageBridgeSSETransformer{
		u: u, r: r, reqID: reqID, repairer: repairer,
		tracked: map[string]*bridgeCallItem{}, executed: map[string]bool{},
		results: map[string][]any{}, syntheticIdx: syntheticIndexBase,
	}
}

// Push 处理一个 chunk，返回可写给客户端的 SSE 文本。
func (t *imageBridgeSSETransformer) Push(chunk string) string {
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
func (t *imageBridgeSSETransformer) Flush() string {
	if t.pending == "" {
		return ""
	}
	rest := t.pending
	t.pending = ""
	return t.transformEvents(rest)
}

// transformEvents 逐帧处理完整 SSE 事件。
func (t *imageBridgeSSETransformer) transformEvents(text string) string {
	var out strings.Builder
	for _, frame := range parseSSEFrames(text) {
		out.WriteString(t.transformEvent(frame.event, frame.data))
	}
	return out.String()
}

// transformEvent 处理单个 SSE 事件。
// 按项身份路由：bridge_imagegen 的帧由本 transformer 处理，其余（含降级成 function_call
// 的 custom 工具）交给内嵌 repairer——不能按"data 里含 function_call 字样"粗判，
// 否则 custom 工具降级修理在 bridge 开启时整体失效。
func (t *imageBridgeSSETransformer) transformEvent(eventName, data string) string {
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
		if isBridgeCall(item) {
			key := stringifyAny(item["id"])
			t.tracked[key] = &bridgeCallItem{id: key, outputIndex: outputIndex}
			return sseFrame(eventName, payload)
		}
		// 非 bridge 项 → custom 工具修理器（或原样透传）
		if t.repairer != nil {
			return t.repairer.transformEvent(eventName, data)
		}
		return sseFrame(eventName, payload)

	case "response.function_call_arguments.delta":
		itemID := stringifyAny(payload["item_id"])
		if call, ok := t.tracked[itemID]; ok {
			call.argumentsText += stringifyAny(payload["delta"])
			return ""
		}
		if t.repairer != nil {
			return t.repairer.transformEvent(eventName, data)
		}
		return sseFrame(eventName, payload)

	case "response.function_call_arguments.done":
		itemID := stringifyAny(payload["item_id"])
		call, ok := t.tracked[itemID]
		if !ok {
			if t.repairer != nil {
				return t.repairer.transformEvent(eventName, data)
			}
			return sseFrame(eventName, payload)
		}
		if args, ok := payload["arguments"].(string); ok && args != "" {
			call.argumentsText = args
		}
		return t.emitBridgeResult(call)

	case "response.output_item.done":
		item, _ := payload["item"].(map[string]any)
		if isBridgeCall(item) {
			key := stringifyAny(item["id"])
			if t.executed[key] {
				// emitBridgeResult 已合成过 done：吸收这条真实 done，避免重复
				delete(t.tracked, key)
				return ""
			}
			call, ok := t.tracked[key]
			if !ok {
				return sseFrame(eventName, payload)
			}
			// 没走 arguments.done（上游直接发完整 item）：用完整 arguments 执行
			if args, ok := item["arguments"].(string); ok && args != "" {
				call.argumentsText = args
			}
			return t.emitBridgeResult(call)
		}
		if t.repairer != nil {
			return t.repairer.transformEvent(eventName, data)
		}
		return sseFrame(eventName, payload)

	case "response.completed", "response.incomplete", "response.failed":
		response, _ := payload["response"].(map[string]any)
		if response == nil {
			if t.repairer != nil {
				return t.repairer.transformEvent(eventName, data)
			}
			return sseFrame(eventName, payload)
		}
		// 1) 先修 custom 工具：completed output 里仍是上游原始的降级 function_call
		repairedResponse := response
		if t.repairer != nil {
			if repaired, _ := repairResponsesBodyToolShapes(response, t.repairer.customTools); repaired != nil {
				repairedResponse = repaired
			}
		}
		// 2) 已执行的 bridge 调用：把合成项（image_generation_call + function_call_output）
		//    并入 completed output，保证以 completed 为权威重建的客户端不丢图片结果。
		//    未执行的 bridge 调用交给 interceptBridgeNonStreaming 补执行。
		mergedOutput, merged := mergeExecutedBridgeResults(repairedResponse["output"], t.executed, t.results)
		nextResponse := cloneMap(repairedResponse)
		if merged {
			nextResponse["output"] = mergedOutput
		}
		if b, err := json.Marshal(map[string]any{"output": nextResponse["output"]}); err == nil {
			if rewritten := t.u.interceptBridgeNonStreaming(b, t.r, t.reqID, t.executed); rewritten != nil {
				var doc map[string]any
				if json.Unmarshal(rewritten, &doc) == nil {
					next := cloneMap(payload)
					nextResponse["output"] = doc["output"]
					next["response"] = nextResponse
					return sseFrame(eventName, next)
				}
			}
		}
		return sseFrame(eventName, payload)
	}
	return sseFrame(eventName, payload)
}

// isBridgeCall 判断响应项是否为 bridge_imagegen 的 function_call。
func isBridgeCall(item map[string]any) bool {
	if item == nil {
		return false
	}
	return stringifyAny(item["type"]) == "function_call" && stringifyAny(item["name"]) == bridgeImagegenToolName
}

// emitBridgeResult 执行 bridge 调用并把结果作为事件发回。
// 收尾原 function_call 项 + 成功时发 image_generation_call 与 function_call_output；
// 参数解析失败 / 上游图片 API 出错时发错误工具输出，避免客户端卡在未完成的 function_call。
func (t *imageBridgeSSETransformer) emitBridgeResult(call *bridgeCallItem) string {
	if t.executed[call.id] {
		return ""
	}
	t.executed[call.id] = true
	delete(t.tracked, call.id)
	args := parseImageBridgeArgs(call.argumentsText)

	// 收尾原 function_call 项（始终发，标记调用已完成）
	var out strings.Builder
	out.WriteString(sseFrame("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": call.outputIndex,
		"item": map[string]any{"type": "function_call", "name": bridgeImagegenToolName,
			"id": call.id, "status": "completed", "arguments": call.argumentsText},
	}))

	if args == nil {
		t.u.log.Warnf("bridge_imagegen_bad_args req=%d id=%s", t.reqID, call.id)
		errItem := bridgeErrorOutputItem(call.id, "工具参数无法解析")
		t.results[call.id] = []any{errItem}
		return out.String() + sseFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": t.allocSynthetic(), "item": errItem,
		}) + sseFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": t.allocSynthetic(), "item": errItem,
		})
	}
	res, err := t.u.executeImageBridge(args, t.r, t.reqID)
	if err != nil {
		t.u.log.Warnf("bridge_imagegen_exec_failed req=%d err=%v", t.reqID, err)
		errItem := bridgeErrorOutputItem(call.id, err.Error())
		t.results[call.id] = []any{errItem}
		return out.String() + sseFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": t.allocSynthetic(), "item": errItem,
		}) + sseFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": t.allocSynthetic(), "item": errItem,
		})
	}
	t.u.log.Infof("bridge_imagegen_executed req=%d saved=%s", t.reqID, res.imagePath)
	handler := &imageBridgeHandler{u: t.u, r: t.r, reqID: t.reqID}
	imageItem := handler.buildImageGenerationItem(res, args, "ig_"+call.id)
	output := fmt.Sprintf("Image generated successfully. The generated image is available at %s.", res.imagePath)

	// 图片生成项（独立分配 output_index，不与上游冲突）
	imageIdx := t.allocSynthetic()
	out.WriteString(sseFrame("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": imageIdx, "item": imageItem,
	}))
	out.WriteString(sseFrame("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": imageIdx, "item": imageItem,
	}))
	// 工具输出项
	outputItem := map[string]any{
		"type": "function_call_output", "call_id": call.id, "output": output,
	}
	t.results[call.id] = []any{imageItem, outputItem}
	outputIdx := t.allocSynthetic()
	out.WriteString(sseFrame("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": outputIdx, "item": outputItem,
	}))
	out.WriteString(sseFrame("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": outputIdx, "item": outputItem,
	}))
	return out.String()
}

// bridgeErrorOutputItem 构造带错误信息的 function_call_output 输出项。
func bridgeErrorOutputItem(callID, msg string) map[string]any {
	return map[string]any{
		"type": "function_call_output", "call_id": callID,
		"output": fmt.Sprintf("bridge_imagegen failed: %s", msg),
	}
}

// mergeExecutedBridgeResults 把已执行的 bridge 调用的合成结果项并入 output：
// 上游原始 function_call 替换成流内已下发的 image_generation_call + function_call_output。
func mergeExecutedBridgeResults(output any, executed map[string]bool, results map[string][]any) ([]any, bool) {
	arr, ok := output.([]any)
	if !ok || len(results) == 0 {
		return arr, false
	}
	next := make([]any, 0, len(arr))
	changed := false
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m != nil && isBridgeCall(m) && executed[stringifyAny(m["id"])] {
			if synth := results[stringifyAny(m["id"])]; len(synth) > 0 {
				next = append(next, synth...)
				changed = true
				continue
			}
		}
		next = append(next, item)
	}
	return next, changed
}

// imageBridgeResponseWriter 处理响应里的 bridge_imagegen 调用：执行并把结果改写进响应。
// 非流式路径（interceptBridgeResponse）与 SSE 路径（imageBridgeSSETransformer）共用执行逻辑。
type imageBridgeHandler struct {
	u     *Upstream
	r     *http.Request
	reqID int64
}

// buildImageGenerationItem 构造 image_generation_call 输出项。
func (h *imageBridgeHandler) buildImageGenerationItem(res *imageBridgeResult, args *imageBridgeArgs, id string) map[string]any {
	revised := res.revisedPrompt
	if revised == "" {
		revised = args.Prompt
	}
	return map[string]any{
		"id":             id,
		"type":           "image_generation_call",
		"status":         "completed",
		"action":         args.Action,
		"output_format":  orString(args.OutputFormat, h.u.cfg.ImageOutputFormat),
		"quality":        orString(args.Quality, h.u.cfg.ImageQuality),
		"result":         res.b64JSON,
		"revised_prompt": revised,
		"size":           orString(args.Size, h.u.cfg.ImageSize),
	}
}

// interceptBridgeNonStreaming 处理非流式 Responses 响应里的 bridge_imagegen 调用。
// 把 function_call(bridge_imagegen) 项替换成 image_generation_call + function_call_output。
// skipCalls 非空时（SSE 路径的 completed 帧）：已在流里执行过的调用 id 跳过，不重复执行
// （SSE 的 emitBridgeResult 已发过结果，completed 的 output 里仍含原始 function_call）。
func (u *Upstream) interceptBridgeNonStreaming(body []byte, r *http.Request, reqID int64, skipCalls map[string]bool) []byte {
	if !u.cfg.BridgeImagegenEnabled {
		return body
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	output, ok := doc["output"].([]any)
	if !ok {
		return body
	}
	handler := &imageBridgeHandler{u: u, r: r, reqID: reqID}
	changed := false
	next := make([]any, 0, len(output)+1)
	for _, item := range output {
		m, _ := item.(map[string]any)
		if m == nil || stringifyAny(m["type"]) != "function_call" ||
			stringifyAny(m["name"]) != bridgeImagegenToolName {
			next = append(next, item)
			continue
		}
		// 已在 SSE 流里执行过的调用：保留原始 function_call 项，不重复执行
		if skipCalls != nil && skipCalls[stringifyAny(m["id"])] {
			next = append(next, item)
			continue
		}
		args := parseImageBridgeArgs(stringifyAny(m["arguments"]))
		if args == nil {
			next = append(next, item)
			continue
		}
		res, err := u.executeImageBridge(args, r, reqID)
		if err != nil {
			u.log.Warnf("bridge_imagegen_exec_failed req=%d err=%v", reqID, err)
			next = append(next, item)
			continue
		}
		callID := stringifyAny(m["call_id"])
		if callID == "" {
			callID = stringifyAny(m["id"])
		}
		imageItem := handler.buildImageGenerationItem(res, args, "ig_"+callID)
		next = append(next, imageItem)
		next = append(next, map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  fmt.Sprintf("Image generated successfully. The generated image is available at %s.", res.imagePath),
		})
		changed = true
		u.log.Infof("bridge_imagegen_executed req=%d saved=%s", reqID, res.imagePath)
	}
	if !changed {
		return body
	}
	out := make(map[string]any, len(doc)+1)
	for k, v := range doc {
		out[k] = v
	}
	out["output"] = next
	b, err := json.Marshal(out)
	if err != nil {
		return body
	}
	return b
}

// decodeBase64 解码 base64 字符串；失败返回 nil。
func decodeBase64(data string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	return decoded
}

// cloneMap 浅拷贝一个 map（避免改写共享的响应对象）。
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// randomSuffix 返回 6 位十六进制随机后缀（crypto/rand，避免基于纳秒的伪随机被预测）。
func randomSuffix() string {
	const hex = "0123456789abcdef"
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败基本不可能；退化为纳秒派生，保证不 panic。
		for i := range b {
			b[i] = hex[time.Now().UnixNano()%16]
		}
		return string(b)
	}
	out := make([]byte, 6)
	for i, v := range b {
		out[i] = hex[int(v)%16]
	}
	return string(out)
}

// orString 返回 a 非空时的值，否则 b。
func orString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// mustJSON 序列化 v 为 JSON；失败返回空对象串（不应发生）。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
