package proxy

import (
	"encoding/json"
	"strings"
)

// isImageUnsupportedError 判断上游错误体是否为"不支持图片输入"的拒绝。
//
// 依据真实网关实测（2026-09-09，GLM-5.2 发图片）：
//
//	HTTP 400，body:
//	{"error":{"message":"Model do not support image input. ...",
//	          "type":"BadRequest","param":"image_url","code":"InvalidParameter"}}
//
// 同时兼容其他网关的常见写法：
//   - "only support text"（火山方舟，不提 image，且常缺三单 s）
//   - image/vision/multimodal/modality/media 等关键词 + 拒绝语气
func isImageUnsupportedError(body string) bool {
	lower := strings.ToLower(body)
	// 自证性表述：本身就是"仅支持文本"断言，无需出现 image 字样
	for _, hint := range []string{"only support text", "only supports text",
		"do not support image", "does not support image",
		"not support image", "doesn't support image",
		"image input"} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	// 含视觉/多模态词汇的"不支持"类错误，含中文词汇
	for _, media := range []string{"image", "vision", "multimodal", "multi-modal",
		"modality", "modalities", "media", "图片", "图像", "视觉", "多模态"} {
		if strings.Contains(lower, media) {
			for _, reject := range []string{"not support", "unsupported", "invalid",
				"不支持", "无法", "暂不", "仅支持文本"} {
				if strings.Contains(lower, reject) {
					return true
				}
			}
		}
	}
	return false
}

// rewriteBodyOmitImages 把图片块替换为占位文本，根据适配器类型选择正确的转换函数。
// originalBody 是适配前的原始 Responses body（快照，未经 prepareAdapter 改写）；重新解析并以
// OmitImages=true 适配。返回改写后的 body 和是否有实际修改。
func rewriteBodyOmitImages(originalBody []byte, adapt *adapterContext) ([]byte, bool) {
	if len(originalBody) == 0 || adapt == nil {
		return nil, false
	}
	var doc map[string]any
	if err := json.Unmarshal(originalBody, &doc); err != nil {
		return nil, false
	}
	// 模型名用适配器归一后的名字：originalBody 是 prepareAdapter 改写前的快照，
	// doc["model"] 仍是客户端原始拼写，而首次适配发给上游的是归一后的名字。
	// 不覆盖的话，走别名表的模型（如 claude-opus-5 → "Opus 5"）在重试时会带上游不认的
	// 拼写，图片降级从"挽救一次 400"变成"再吃一个 400/404"。
	if adapt.normalizedModel != "" {
		doc["model"] = adapt.normalizedModel
	}
	// 检查原始请求是否含图片，没有就不改写（避免无谓的解析）
	if !requestHasImages(doc) {
		return nil, false
	}
	// 这里拿的是**适配前**的原始 Responses body，工具可能还在 input 的 additional_tools 载体里
	// （responses lite 形状）。必须与 prepareAdapter 一样先展开，否则重试请求会一个工具都不带。
	// 回程还原表由 adapt.toolNamespaces 提供（首次适配时已算好），此处不必再传。
	if _, err := applyCodexToolContext(doc); err != nil {
		// 工具名冲突等无法展开：放弃改写，让调用方交付上游的原始错误响应。
		return nil, false
	}
	// 重新适配，强制全部省略图片（含当前轮次的图片）
	// OmitImages=true：历史图片走 historicalImageText（语义一致）；
	// SupportsImages=false：当前轮次图片走 omittedImageText。
	opts := adapterOptions{SupportsImages: false, OmitImages: true}
	var converted map[string]any
	var err error
	if adapt.isChatAdapter {
		converted, err = responsesToChatRequest(doc, opts)
	} else {
		converted, err = responsesToMessagesRequest(doc, opts)
	}
	if err != nil {
		return nil, false
	}
	b, err := json.Marshal(converted)
	if err != nil {
		return nil, false
	}
	return b, true
}

// requestHasImages 快速判断 Responses 请求的 input 里是否含图片块。
func requestHasImages(doc map[string]any) bool {
	input, ok := doc["input"].([]any)
	if !ok {
		if s, ok := doc["input"].(string); ok && s != "" {
			return false
		}
		return false
	}
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if t := stringifyAny(m["type"]); t == "input_image" || t == "image" {
			return true
		}
		content, ok := m["content"].([]any)
		if !ok {
			if s := stringifyAny(m["content"]); s != "" {
				continue
			}
			continue
		}
		for _, block := range content {
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t := stringifyAny(bm["type"]); t == "input_image" || t == "image" {
				return true
			}
		}
	}
	return false
}
