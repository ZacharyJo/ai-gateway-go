package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// gAAAA 开头的加密串（AESGCM 加密态），诊断时脱敏。
var gaaaaPattern = regexp.MustCompile(`gAAAA[A-Za-z0-9_=-]{20,}`)

// encryptedContentPattern 匹配 "encrypted_content" 字段值，脱敏用。
var encryptedContentPattern = regexp.MustCompile(`"encrypted_content"\s*:\s*"([^"]*)"`)

// captureInfo 是一次转发的捕获上下文（CAPTURE_ERROR_BODIES=1 且上游状态 >= 400 时落盘）。
type captureInfo struct {
	enabled      bool
	dir          string
	reqID        int64
	method       string
	path         string
	reqBody      []byte
	upstreamBase string
}

// writeCapture 把错误请求/响应落盘到 <dir>/ai-gateway-captures/<stamp>-req-<id>-status-<status>.json。
// 落盘失败不影响响应（fail-open）。
func (c *captureInfo) write(status int, respBody []byte) {
	if c == nil || !c.enabled || c.dir == "" {
		return
	}
	stamp := time.Now().UTC().Format("2006-01-02T15-04-05.000Z")
	name := fmt.Sprintf("%s-req-%d-status-%d.json", stamp, c.reqID, status)
	path := filepath.Join(c.dir, "ai-gateway-captures", name)
	payload := map[string]any{
		"capturedAt":   time.Now().Format(time.RFC3339),
		"upstreamBase": c.upstreamBase,
		"request": map[string]any{
			"method":    c.method,
			"path":      c.path,
			"bodyBytes": len(c.reqBody),
			"body":      redact(string(c.reqBody)),
		},
		"response": map[string]any{
			"status":    status,
			"bodyBytes": len(respBody),
			"bodyText":  redact(string(respBody)),
		},
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return
	}
}

// redact 脱敏加密内容：gAAAA 串与 encrypted_content 字段值替换为长度标记。
func redact(s string) string {
	if s == "" {
		return s
	}
	s = gaaaaPattern.ReplaceAllStringFunc(s, func(m string) string {
		return fmt.Sprintf("[redacted gAAAA length=%d]", len(m))
	})
	s = encryptedContentPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := encryptedContentPattern.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		return fmt.Sprintf(`"encrypted_content": "[redacted encrypted_content length=%d]"`, len(sub[1]))
	})
	return s
}
