package proxy

import (
	"encoding/json"
	"regexp"
	"strings"
)

// 流内软错误检测。
// 上游有时把「并发超限」「模型容量不足」这类错误塞进 HTTP 200 的 SSE 流里，
// 客户端不认识就表现为对话莫名中断且不会重试。检测到后向下游写一帧 Codex 认识的
// server_overloaded 错误并结束流，把重试交给客户端（它自己有 stream_max_retries）。

// exceptionalSSEPattern 是廉价预筛：只有可能含错误的 chunk 才做重型 JSON 解析。
var exceptionalSSEPattern = regexp.MustCompile(`(?i)event:\s*error|"type"\s*:\s*"error"|"type"\s*:\s*"response\.failed"|"status"\s*:\s*"failed"|"status_details"\s*:|"error"\s*:|"last_error"\s*:`)

// 判定短语（必须同时命中才算）。
var (
	reConcurrencyLimit = regexp.MustCompile(`(?i)concurrency limit exceeded for account`)
	reRetryLater       = regexp.MustCompile(`(?i)please retry later`)
	reModelAtCapacity  = regexp.MustCompile(`(?i)selected model is at capacity`)
	reTryDifferent     = regexp.MustCompile(`(?i)please try a different model`)
	reServerOverloaded = regexp.MustCompile(`(?i)server_is_overloaded`)
	reServersOverload  = regexp.MustCompile(`(?i)servers?\s+.*overloaded`)
	reTryAgainLater    = regexp.MustCompile(`(?i)please try again later`)
	// 终态（不可重试）错误：上游按 Prompt 指纹做防重放，命中后该 prompt 进 ~25min 冷却。
	// 对它重试是必然失败且会延长冷却，故绝不能转成 server_overloaded 诱导客户端重试。
	reFingerprintCooldown = regexp.MustCompile(`(?i)fingerprint_replay_cooldown|重放冷却`)
)

// terminalStreamErrorKind 是命中后应当作终态透传（不发 server_overloaded、不诱导客户端重试）的错误类型。
const kindFingerprintCooldown = "fingerprint_cooldown"

// isTerminalStreamErrorKind 判断软错误检测出的 kind 是否为终态（不可重试）类型。
func isTerminalStreamErrorKind(kind string) bool {
	return kind == kindFingerprintCooldown
}

// errorContainerKeys 是承载错误对象的字段名；只在这些字段内部收集文本，避免正常内容误命中。
var errorContainerKeys = map[string]bool{
	"error": true, "last_error": true, "status_details": true, "response": true,
}

// errorTextKeys 是错误对象里的文本字段名。
var errorTextKeys = map[string]bool{
	"message": true, "type": true, "code": true, "param": true,
}

// streamErrorBufferBytes 是检测缓冲上限。
const streamErrorBufferBytes = 256 * 1024

// streamErrorDetector 跨 chunk 累积文本并判定是否为可重试软错误。
type streamErrorDetector struct {
	buf  []byte
	kind string // 命中后固定，不再重复判定
}

// feed 累积一个 chunk 并返回命中的软错误类型（concurrency_limit / model_capacity），未命中返回空串。
func (d *streamErrorDetector) feed(chunk []byte) string {
	if d.kind != "" {
		return d.kind
	}
	d.buf = append(d.buf, chunk...)
	if len(d.buf) > streamErrorBufferBytes {
		d.buf = d.buf[len(d.buf)-streamErrorBufferBytes:]
	}
	// 预筛直接在 []byte 上做：转 string 会整段拷贝最多 256KB，长流下是
	// O(chunk 数 × 256KB) 的无谓拷贝，而绝大多数 chunk 根本不含错误特征。
	// 只有命中才付一次转换成本。
	if !exceptionalSSEPattern.Match(d.buf) {
		return ""
	}
	d.kind = retryableKindFromSSE(string(d.buf))
	return d.kind
}

// retryableKindFromSSE 解析 SSE 文本里的 data: JSON，收集错误文本后判定类型。
func retryableKindFromSSE(text string) string {
	var texts []string
	for _, frame := range parseSSEFrames(text) {
		data := strings.TrimSpace(frame.data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			continue
		}
		collectErrorTexts(parsed, false, &texts)
	}
	if len(texts) == 0 {
		return ""
	}
	joined := strings.Join(texts, "\n")
	switch {
	case reFingerprintCooldown.MatchString(joined):
		return kindFingerprintCooldown // 终态：不可重试
	case reConcurrencyLimit.MatchString(joined) && reRetryLater.MatchString(joined):
		return "concurrency_limit"
	case reModelAtCapacity.MatchString(joined) && reTryDifferent.MatchString(joined),
		reServerOverloaded.MatchString(joined),
		reServersOverload.MatchString(joined) && reTryAgainLater.MatchString(joined):
		return "model_capacity"
	}
	return ""
}

// collectErrorTexts 递归收集错误文本：只有进入 errorContainerKeys 之后才采集 errorTextKeys 的字符串值。
func collectErrorTexts(value any, inErrorCtx bool, out *[]string) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			childCtx := inErrorCtx || errorContainerKeys[key]
			if inErrorCtx && errorTextKeys[key] {
				if s, ok := child.(string); ok && s != "" {
					*out = append(*out, s)
					continue
				}
			}
			collectErrorTexts(child, childCtx, out)
		}
	case []any:
		for _, child := range v {
			collectErrorTexts(child, inErrorCtx, out)
		}
	}
}

// overloadedErrorFrame 是写给下游的错误帧：Codex 认识 server_overloaded 并会按自己的
// stream_max_retries 重试（流内错误时下发的帧）。
const overloadedErrorFrame = "event: error\n" +
	`data: {"type":"error","error":{"type":"server_error","code":"server_overloaded","message":"server overloaded"}}` +
	"\n\n"
