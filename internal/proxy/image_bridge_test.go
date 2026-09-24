package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseImageBridgeArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *imageBridgeArgs
	}{
		{"plain", `{"action":"generate","prompt":"a red fox"}`, &imageBridgeArgs{Action: "generate", Prompt: "a red fox"}},
		{"fenced", "```json\n{\"action\":\"edit\",\"prompt\":\"add clouds\",\"source_image_path\":\"/tmp/a.png\"}\n```",
			&imageBridgeArgs{Action: "edit", Prompt: "add clouds", SourceImagePath: "/tmp/a.png"}},
		{"no action defaults generate", `{"prompt":"hello"}`, &imageBridgeArgs{Action: "generate", Prompt: "hello"}},
		{"truncated object", `{"action":"generate","prompt":"a red fox","size":`, &imageBridgeArgs{Action: "generate", Prompt: "a red fox"}},
		{"garbage", `not json`, nil},
		{"empty", ``, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseImageBridgeArgs(c.in)
			if c.want == nil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil || got.Prompt != c.want.Prompt || got.Action != c.want.Action {
				t.Fatalf("parse = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestInjectBridgeImagegenTool(t *testing.T) {
	cfg := &Config{ImageModel: "gpt-image-2.5-sunburst", ImageSize: "auto", ImageQuality: "medium", ImageOutputFormat: "png"}
	doc := map[string]any{
		"model": "gpt-5.5",
		"input": []any{map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "go"}}}},
	}
	if !injectBridgeImagegenTool(doc, cfg) {
		t.Fatalf("expected injection")
	}
	tools := doc["tools"].([]any)
	found := false
	for _, t := range tools {
		if m, ok := t.(map[string]any); ok && m["name"] == bridgeImagegenToolName {
			found = true
		}
	}
	if !found {
		t.Fatalf("bridge_imagegen tool not injected: %v", tools)
	}
	// 幂等：再次注入不重复
	if injectBridgeImagegenTool(doc, cfg) {
		t.Fatalf("second injection should be no-op")
	}
	// 指令应注入到 developer 消息
	input := doc["input"].([]any)
	first := input[0].(map[string]any)
	if first["role"] != "developer" {
		t.Fatalf("expected developer instruction message first, got %v", first["role"])
	}
}

func TestInstallImagegenSkill(t *testing.T) {
	dir := t.TempDir()
	installed, err := installImagegenSkill(dir)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(installed) < 3 {
		t.Fatalf("expected >=3 files, got %d: %v", len(installed), installed)
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scripts", "image_gen.py")); err != nil {
		t.Fatalf("image_gen.py not installed: %v", err)
	}
	// 幂等：再装一次不报错（不覆盖）
	if _, err := installImagegenSkill(dir); err != nil {
		t.Fatalf("re-install: %v", err)
	}
}

// TestImageBridgeSSETransformer 用 mock 上游图片 API 端到端验证 SSE 桥接执行：
// 模型调 bridge_imagegen → 代理转调上游 → 落盘 → 输出 image_generation_call + function_call_output。
func TestImageBridgeSSETransformer(t *testing.T) {
	// mock 上游图片 API：返回一张假 b64 图片
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		if r.URL.Path != "/images/generations" {
			t.Errorf("path = %s, want /images/generations", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("fake-image-bytes")) + `","revised_prompt":"a cat"}]}`))
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	logDir := t.TempDir()
	u := &Upstream{
		cfg: &Config{
			BridgeImagegenEnabled: true,
			ImageModel:            "gpt-image-test",
			ImageSize:             "1024x1024",
			ImageQuality:          "medium",
			ImageOutputFormat:     "png",
			LogDir:                logDir,
			AuthMode:              "none",
		},
		base:   base,
		client: &http.Client{Timeout: 5 * time.Second},
		log:    NewLogger(io.Discard, false),
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://127.0.0.1/v1/responses", nil)
	bt := newImageBridgeSSETransformer(u, req, 1, nil)

	events := []string{
		sseFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"type": "function_call", "name": "bridge_imagegen", "id": "fc_1", "arguments": ""},
		}),
		sseFrame("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": `{"action":"generate","prompt":"a c`,
		}),
		sseFrame("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": "fc_1", "arguments": `{"action":"generate","prompt":"a cat"}`,
		}),
	}

	var out strings.Builder
	out.WriteString(bt.Push(events[0]))
	out.WriteString(bt.Push(events[1] + events[2]))
	out.WriteString(bt.Flush())

	text := out.String()
	if !hit {
		t.Fatalf("mock image API not called")
	}
	// 不能泄漏原始 arguments.delta/done 帧
	if strings.Contains(text, "response.function_call_arguments") {
		t.Fatalf("arguments frames leaked:\n%s", text)
	}
	for _, want := range []string{"image_generation_call", `"type":"function_call_output"`, "Image generated successfully"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
	// 图片应落盘
	files, _ := os.ReadDir(filepath.Join(logDir, "generated-images"))
	if len(files) == 0 {
		t.Fatalf("no generated image persisted")
	}
	// 所有帧仍是合法 SSE + JSON
	for _, f := range parseSSEFrames(text) {
		if f.data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(f.data), &m); err != nil {
			t.Fatalf("frame %s invalid json: %v", f.event, err)
		}
	}
}

func TestInjectBridgeImagegenMessages(t *testing.T) {
	body := []byte(`{"model":"GLM-5.2","system":"you are helpful","messages":[{"role":"user","content":"hi"}]}`)
	out, changed := injectBridgeImagegenMessages(body)
	if !changed {
		t.Fatalf("expected injection")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	tools := doc["tools"].([]any)
	if len(tools) != 1 || stringifyAny(tools[0].(map[string]any)["name"]) != bridgeImagegenToolName {
		t.Fatalf("tool not injected: %v", tools)
	}
	if sys := stringifyAny(doc["system"]); !strings.Contains(sys, bridgeImagegenToolName) {
		t.Fatalf("system instruction missing: %q", sys)
	}
	// 幂等
	if _, ok := injectBridgeImagegenMessages(out); ok {
		t.Fatalf("second injection should be no-op")
	}
}

func TestInjectBridgeImagegenChat(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	out, changed := injectBridgeImagegenChat(body)
	if !changed {
		t.Fatalf("expected injection")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	tools := doc["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if stringifyAny(fn["name"]) != bridgeImagegenToolName {
		t.Fatalf("tool not injected: %v", tools)
	}
	// 指令应进首条 system 消息
	messages := doc["messages"].([]any)
	first := messages[0].(map[string]any)
	if stringifyAny(first["role"]) != "system" || !strings.Contains(stringifyAny(first["content"]), bridgeImagegenToolName) {
		t.Fatalf("system message missing: %v", messages[0])
	}
	// 幂等
	if _, ok := injectBridgeImagegenChat(out); ok {
		t.Fatalf("second injection should be no-op")
	}
}

// TestChainedSSETransformer 验证链式组合：second 消费 first 的输出。
func TestChainedSSETransformer(t *testing.T) {
	first := &passThroughTransformer{}
	second := newToolShapeRepairer(map[string]bool{"exec": true})
	chained := &chainedSSETransformer{first: first, second: second}

	frame := sseFrame("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "function_call", "name": "exec", "id": "fc_1", "arguments": `{"input":"ls"}`},
	})
	out := chained.Push(frame) + chained.Flush()
	if !strings.Contains(out, "custom_tool_call") {
		t.Fatalf("chained second transformer did not repair:\n%s", out)
	}
}

// passThroughTransformer 原样透传（测试链式组合的 first 端）。
type passThroughTransformer struct{}

func (p *passThroughTransformer) Push(chunk string) string { return chunk }
func (p *passThroughTransformer) Flush() string            { return "" }

// TestInterceptBridgeNonStreamingSkip 验证 SSE completed 帧防重复执行：
// skipCalls 里的调用 id 不再次转调上游图片 API。
func TestInterceptBridgeNonStreamingSkip(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("fake")) + `"}]}`))
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	u := &Upstream{
		cfg: &Config{BridgeImagegenEnabled: true, ImageModel: "m", ImageSize: "1024x1024",
			ImageQuality: "medium", ImageOutputFormat: "png", LogDir: t.TempDir(), AuthMode: "none"},
		base:   base,
		client: &http.Client{Timeout: 5 * time.Second},
		log:    NewLogger(io.Discard, false),
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://127.0.0.1/v1/responses", nil)
	body := []byte(`{"output":[{"type":"function_call","name":"bridge_imagegen","id":"fc_1","arguments":"{\"prompt\":\"a cat\"}"}]}`)

	// 无 skip：执行一次
	_ = u.interceptBridgeNonStreaming(body, req, 1, nil)
	if hits != 1 {
		t.Fatalf("first call hits = %d, want 1", hits)
	}
	// 带 skip：不再执行
	_ = u.interceptBridgeNonStreaming(body, req, 1, map[string]bool{"fc_1": true})
	if hits != 1 {
		t.Fatalf("skipped call hits = %d, want still 1 (double-execution bug)", hits)
	}
}

// TestExecuteImageBridgeURLResponse 验证上游返回图片 URL（而非 b64_json）时，
// 桥接会下载图片并落盘到 generated-images/（第三方中转普遍返回 url）。
func TestExecuteImageBridgeURLResponse(t *testing.T) {
	// 图片文件端点：返回一个假 JPEG 字节
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0fake-jpeg-bytes"))
	}))
	defer imgSrv.Close()

	// 图片 API：返回 url 而非 b64_json
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Errorf("path = %s, want /images/generations", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"` + imgSrv.URL + `/img.jpeg","mime_type":"image/jpeg"}]}`))
	}))
	defer apiSrv.Close()

	base, _ := url.Parse(apiSrv.URL)
	logDir := t.TempDir()
	u := &Upstream{
		cfg: &Config{
			BridgeImagegenEnabled: true,
			ImageModel:            "grok-imagine-image-2.0",
			ImageSize:             "1024x1024",
			ImageQuality:          "medium",
			ImageOutputFormat:     "jpeg",
			LogDir:                logDir,
			AuthMode:              "none",
		},
		base:   base,
		client: &http.Client{Timeout: 5 * time.Second},
		log:    NewLogger(io.Discard, false),
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://127.0.0.1/v1/responses", nil)
	res, err := u.executeImageBridge(&imageBridgeArgs{Action: "generate", Prompt: "a cat"}, req, 1)
	if err != nil {
		t.Fatalf("executeImageBridge: %v", err)
	}
	if res.imagePath == "" {
		t.Fatal("imagePath empty")
	}
	// 落盘内容应与图片端点字节一致（下载而非空）
	raw, err := os.ReadFile(res.imagePath)
	if err != nil {
		t.Fatalf("read persisted image: %v", err)
	}
	if !strings.Contains(string(raw), "fake-jpeg-bytes") {
		t.Errorf("persisted image bytes mismatch: %q", raw)
	}
	// URL 形态也必须内嵌 base64 到 result，否则 codex 拿到空图
	if res.b64JSON == "" {
		t.Error("b64JSON empty for URL response (client would receive a blank image)")
	}
	if want := base64.StdEncoding.EncodeToString(raw); res.b64JSON != want {
		t.Errorf("b64JSON mismatch: got len=%d want len=%d", len(res.b64JSON), len(want))
	}
}

// TestDownloadBridgeImageSchemeWhitelist 验证只允许 http/https 图片 URL（SSRF 防御）。
func TestDownloadBridgeImageSchemeWhitelist(t *testing.T) {
	up := &Upstream{cfg: &Config{}, log: NewLogger(io.Discard, false)}
	for _, bad := range []string{"file:///etc/passwd", "ftp://example.com/img.png", "data:image/png;base64,AAAA"} {
		if _, err := up.downloadBridgeImage(bad); err == nil {
			t.Errorf("scheme should be rejected: %s", bad)
		}
	}
}
