package proxy

import "strings"

// 思考内容处理（对齐 cc-switch 的 transform_codex_chat + codex_chat_common）。
//
// 两层来源：
//  1. 独立字段 reasoning_content / reasoning（DeepSeek、Kimi 等思维链模型）；
//  2. 正文 content 里内嵌的 <think>...</think> 前导块（部分模型把思考直接写进正文）。
//
// 目标：思考内容映射成 Responses 协议的独立 `reasoning` 项（summary_text），
// 绝不混进 output_text 正文，正文永远干净。这修复了「content 空时拿 reasoning_content
// 当正文输出，导致 <think>/<review> 标签进正文」的问题。

// think 标签的字面量常量已移除：标签可能带 ing 后缀与属性/空白，一律走 adapter.go 的
// thinkTagName / thinkOpenTagPattern / thinkCloseScan 统一口径，避免各处实现再次漂移。

// extractReasoningText 从一个 message/delta 对象里穷举 reasoning 字段。
// 优先级：reasoning_content(字符串) > reasoning(字符串) > reasoning.{content,text,summary}。
// 不依赖上游 provider 声明，任何 Chat 兼容接口都能兜底。返回去空后仍非空的文本。
func extractReasoningText(m map[string]any) string {
	if m == nil {
		return ""
	}
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if s, ok := m[key].(string); ok && s != "" {
			return cleanReasoningText(s)
		}
	}
	if r, ok := m["reasoning"].(map[string]any); ok {
		for _, key := range []string{"content", "text", "summary"} {
			if s, ok := r[key].(string); ok && s != "" {
				return cleanReasoningText(s)
			}
		}
	}
	return ""
}

// extractReasoningSummaryText 从 Responses `reasoning` item 里提取思考文本。
// 对齐 cc-switch extract_reasoning_summary_text：优先 reasoning_content/content/text，
// 再退到 summary（字符串或 [{text|content|...}] 数组，用 \n\n 拼接）。
func extractReasoningSummaryText(m map[string]any) string {
	if m == nil {
		return ""
	}
	for _, key := range []string{"reasoning_content", "content", "text"} {
		if s, ok := m[key].(string); ok && s != "" {
			return cleanReasoningText(s)
		}
	}
	summary, ok := m["summary"]
	if !ok {
		return ""
	}
	if s, ok := summary.(string); ok {
		return cleanReasoningText(s)
	}
	arr, ok := summary.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, p := range arr {
		switch pv := p.(type) {
		case string:
			if pv != "" {
				parts = append(parts, cleanReasoningText(pv))
			}
		case map[string]any:
			if s, ok := pv["text"].(string); ok && s != "" {
				parts = append(parts, cleanReasoningText(s))
				continue
			}
			if s, ok := pv["content"].(string); ok && s != "" {
				parts = append(parts, cleanReasoningText(s))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// cleanReasoningText 去掉 reasoning 字段里包裹/残留的 think 标签，保留思考正文。
func cleanReasoningText(s string) string {
	return strings.TrimSpace(thinkTagOnlyPattern.ReplaceAllString(s, ""))
}

// splitLeadingThinkBlock 处理正文开头的 <think>...</think> 块：
// 返回 (思考文本, 剩余正文, 命中)。仅当去掉前导空白后以 <think> 开头且能找到闭合标签时命中。
// 未命中返回 ("", "", false)，调用方按原样处理正文。
func splitLeadingThinkBlock(text string) (reasoning, answer string, ok bool) {
	leadingWS := len(text) - len(strings.TrimLeft(text, " \t\r\n"))
	afterWS := text[leadingWS:]
	open := thinkOpenTagPattern.FindStringIndex(afterWS)
	if open == nil {
		return "", "", false
	}
	bodyStart := leadingWS + open[1]
	closing := thinkCloseScan.FindStringIndex(text[bodyStart:])
	if closing == nil {
		return "", "", false
	}
	reasoning = strings.TrimSpace(text[bodyStart : bodyStart+closing[0]])
	answer = strings.TrimLeft(text[bodyStart+closing[1]:], " \t\r\n")
	return reasoning, answer, true
}

// stripLeadingThinkOpenTag 去掉前导 <think>（无闭合标签的兜底路径用）：
// 命中返回 (去标签后文本, true)，未命中返回 ("", false)。
func stripLeadingThinkOpenTag(text string) (string, bool) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	open := thinkOpenTagPattern.FindStringIndex(trimmed)
	if open == nil {
		return "", false
	}
	return strings.TrimLeft(trimmed[open[1]:], " \t\r\n"), true
}

// thinkPrefixDecision 是流式 <think> 前导探测的三态判定。
type thinkPrefixDecision int

const (
	thinkNeedMore  thinkPrefixDecision = iota // 攒够字节前无法判定
	thinkReasoning                            // 确认以 <think> 开头
	thinkText                                 // 确认不是 <think>，按正文处理
)

// inlineThinkMode 是流式内嵌 <think> 处理的状态机（对齐 cc-switch InlineThinkMode）。
type inlineThinkMode int

const (
	inlineThinkDetecting inlineThinkMode = iota // 尚未判定：缓冲前缀等待判定
	inlineThinkReasoning                        // 已确认在 <think> 块内：缓冲直到 </think>
	inlineThinkText                             // 已确认是正文：直接透传
)

// leadingThinkPrefixDecision 判断已缓冲的正文前缀是否为 think 开标签开头。
// 由于标签可能被切在多个 chunk，需要在字节不足时返回 NeedMore 继续缓冲。
func leadingThinkPrefixDecision(buffer string) thinkPrefixDecision {
	trimmed := strings.TrimLeft(buffer, " \t\r\n")
	if trimmed == "" {
		return thinkNeedMore
	}
	if thinkOpenTagPattern.MatchString(trimmed) {
		return thinkReasoning
	}
	if isThinkOpenPartial(trimmed) {
		return thinkNeedMore // 是某个开标签的前缀（如 "<thi" / "<thinking "），可能还没到齐
	}
	return thinkText
}

// isThinkOpenPartial 判断 s（已去前导空白）是否为"尚未写完的 think 开标签"前缀。
// 流式下标签可能被切在多个 chunk（"<thi" + "nking>"），这类尾部必须继续缓冲，
// 否则半个标签会被当正文下发。标签名为 think/thinking 的任意前缀即算命中；
// 出现 '>' 说明标签已完整，交由 thinkOpenTagPattern 判定。
func isThinkOpenPartial(s string) bool {
	if !strings.HasPrefix(s, "<") || strings.ContainsRune(s, '>') {
		return false
	}
	name := strings.ToLower(s[1:])
	if i := strings.IndexAny(name, " \t\r\n"); i >= 0 {
		name = name[:i]
	}
	// 空白紧跟 "<" 时 name 被剥空，这类输入永远长不成标签，直接按正文放行；
	// 只有单独的 "<" 才可能继续长成 <thinking，需要继续缓冲。
	if name == "" {
		return s == "<"
	}
	return len(name) <= len("thinking") &&
		(strings.HasPrefix("think", name) || strings.HasPrefix("thinking", name))
}
