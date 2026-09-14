package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // 应包含
		not  []string // 不应包含
	}{
		// gAAAA 值在 encrypted_content 字段内：gAAAA 先脱敏，encrypted_content 再整值替换（最终只剩后者标记）
		{"gAAAA in encrypted_content", `{"encrypted_content":"gAAAAAbcdef0123456789_=xyz"}`, []string{`"[redacted encrypted_content length=26]"`}, []string{"gAAAAAbcdef0123456789_=xyz"}},
		// 纯 gAAAA 串（非 encrypted_content 字段）：只走 gAAAA 脱敏（标记本身含 gAAAA 字样，不断言子串）
		{"bare gAAAA string", `{"x":"gAAAAAbcdef0123456789_=xyz"}`, []string{"[redacted gAAAA length=26]"}, []string{"gAAAAAbcdef0123456789_=xyz"}},
		// encrypted_content 字段非 gAAAA 值：只走字段脱敏
		{"encrypted_content key", `"encrypted_content":"secret-value"`, []string{"[redacted encrypted_content length=12]"}, []string{"secret-value"}},
		{"normal text untouched", `{"ok":true,"msg":"hello"}`, []string{`{"ok":true,"msg":"hello"}`}, nil},
		{"empty", "", []string{""}, nil},
	}
	for _, c := range cases {
		got := redact(c.in)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: redact(%q) = %q, missing %q", c.name, c.in, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: redact(%q) = %q, should not contain %q", c.name, c.in, got, n)
			}
		}
	}
}

func TestCaptureWrite(t *testing.T) {
	dir := t.TempDir()
	// gAAAA 串需 >= 20 个后续字符才命中脱敏正则
	reqSecret := "gAAAAabcdefghijklmnopqrstuvwxyz0123456789"
	respSecret := "gAAAAABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	c := &captureInfo{
		enabled: true,
		dir:     dir,
		reqID:   42,
		method:  "POST",
		path:    "/v1/responses",
		reqBody: []byte(`{"encrypted_content":"` + reqSecret + `","model":"gpt"}`),
	}
	c.write(500, []byte(`{"error":{"message":"boom `+respSecret+`"}}`))

	files, err := filepath.Glob(filepath.Join(dir, "ai-gateway-captures", "*status-500.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("capture files = %v (err %v), want 1", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	content := string(b)
	// 加密明文不应残留（脱敏标记本身含 "gAAAA" 字样，不能直接查 gAAAA）
	if strings.Contains(content, reqSecret) || strings.Contains(content, respSecret) {
		t.Errorf("capture contains unredacted secret: %s", content)
	}
	if !strings.Contains(content, "[redacted") {
		t.Errorf("capture missing redaction markers: %s", content)
	}
	// 请求/响应都在
	if !strings.Contains(content, "POST") || !strings.Contains(content, "/v1/responses") {
		t.Errorf("capture missing request info: %s", content)
	}
	if !strings.Contains(content, "boom") {
		t.Errorf("capture missing response body: %s", content)
	}
}

func TestCaptureDisabled(t *testing.T) {
	dir := t.TempDir()
	c := &captureInfo{enabled: false, dir: dir, reqID: 1, method: "POST", path: "/x", reqBody: []byte("{}")}
	c.write(500, []byte("boom"))
	files, _ := filepath.Glob(filepath.Join(dir, "ai-gateway-captures", "*"))
	if len(files) != 0 {
		t.Errorf("disabled capture wrote files: %v", files)
	}
}
