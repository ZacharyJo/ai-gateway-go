package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsGptModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-5.6", true},
		{"gpt_4", true},
		{"gpt", true},
		{"GPT-5", true},
		{"GPT 5.6 Luna", true},
		{"  gpt-5.6  ", true},
		{"gpts", false},
		{"gpt j", false},
		{"claude sonnet 5", false},
		{"glm-4.7", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isGptModel(c.model); got != c.want {
			t.Errorf("isGptModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestClassifyModelForResponses(t *testing.T) {
	// GPT 系 → 透传
	if c := classifyModelForResponses("gpt-5.6"); c.IsAdapter() || c.Kind != "gpt_passthrough" {
		t.Errorf("gpt-5.6 = %+v, want gpt_passthrough", c)
	}
	// 支持图片的非 GPT → 适配
	c := classifyModelForResponses("claude sonnet 5")
	if !c.IsAdapter() || c.UpstreamPath != "/messages" {
		t.Fatalf("claude sonnet 5 = %+v, want messages_adapter /messages", c)
	}
	if !c.SupportsImages {
		t.Errorf("claude sonnet 5 images=%v, want true", c.SupportsImages)
	}
	// 已确认只支持文本的模型
	if c2 := classifyModelForResponses("glm-5.2"); c2.SupportsImages {
		t.Errorf("glm-5.2 images=%v, want false", c2.SupportsImages)
	}
	// 未知模型 fail-open：按支持图片处理，让上游给出权威错误
	c3 := classifyModelForResponses("some-unknown-model")
	if !c3.IsAdapter() || !c3.SupportsImages {
		t.Errorf("unknown = %+v, want adapter + images=true (fail-open)", c3)
	}
	// 别名归一
	if !classifyModelForResponses("DeepSeek V4 Pro").IsAdapter() {
		t.Error("alias model should still be adapter")
	}
	if supportsImageInput("Claude  Sonnet   5") != true {
		t.Error("whitespace-folded model name should match capability table")
	}
}

func TestCatalogAlignedModels(t *testing.T) {
	// 与内置能力表对齐：非 GPT、非 /responses 原生的模型都走 Messages 适配
	for _, m := range []string{"gemini 3.7 flash", "GLM-5.3", "Kimi K3", "gemini-3.1-pro"} {
		if c := classifyModelForResponses(m); !c.IsAdapter() {
			t.Errorf("%q should be messages_adapter, got %+v", m, c)
		}
	}
	// GPT 家族仍透传（gpt-6-astra 以 gpt- 开头）
	if c := classifyModelForResponses("gpt-6-astra"); c.IsAdapter() {
		t.Errorf("gpt-6-astra should be passthrough, got %+v", c)
	}
	// 内置表标注支持图片的模型（之前错标成 false，图片会被无故换占位文本）
	for _, m := range []string{"Gemini 3.7 Flash", "Kimi K3", "gemini-3.1-pro"} {
		if !supportsImageInput(m) {
			t.Errorf("%q 内置表标注支持图片，应为 true", m)
		}
	}
	// 内置表标注只支持文本的模型：slug 与展示名两种拼法都要命中
	for _, m := range []string{"DeepSeek-V4-Flash", "DeepSeek V4 Flash", "DeepSeek-V4-Pro",
		"DeepSeek V4 Pro", "GLM-5.2", "GLM-5-Turbo", "GLM-5.3", "auto"} {
		if supportsImageInput(m) {
			t.Errorf("%q 内置表标注不支持图片，应为 false", m)
		}
	}
	// catalog 里的展示名拼法（客户端可能按 display_name 发）
	for _, m := range []string{"Sonnet 5", "Sonnet 4.6", "Haiku 4.5", "Grok 4.5", "Kimi K2.6", "Opus 4.8"} {
		if !supportsImageInput(m) {
			t.Errorf("%q（catalog 展示名）应支持图片", m)
		}
	}
	// catalog 有、内置表未收录的模型：不猜能力，走 fail-open
	for _, m := range []string{"GLM-5.3-Flash", "gpt-6-astra"} {
		if !supportsImageInput(m) {
			t.Errorf("%q 未收录，应 fail-open 而不是猜成不支持图片", m)
		}
	}
	// 内置表有、catalog 暂无：能力条目留着，将来放出来就是对的
	if !supportsImageInput("Opus 4.8") {
		t.Error("Opus 4.8 应支持图片")
	}
	if !supportsImageInput("gemini-3.1-pro") {
		t.Error("gemini-3.1-pro 内置表标注支持图片")
	}
	// 已不在 catalog 的模型：不在表内 → fail-open（不再静默换占位图）
	if !supportsImageInput("fable 5") {
		t.Error("fable 5 已不在 catalog，应 fail-open")
	}
}

func TestResponsesNativePassthrough(t *testing.T) {
	// DeepSeek-V4-Flash 已从 responsesNativeModels 移除：
	// 因为 Codex 的 freeform 工具（apply_patch 等）历史记录里带 custom_tool_call，
	// 上游不认识该类型；只有 Messages 适配路径才能正确处理 freeform 工具的 round-trip。
	for _, m := range []string{"DeepSeek-V4-Flash", "DeepSeek V4 Flash", "deepseek-v4-flash"} {
		c := classifyModelForResponses(m)
		if !c.IsAdapter() {
			t.Errorf("%q 应走 Messages 适配（freeform 工具兼容性），got %+v", m, c)
		}
	}
	// DeepSeek-V4-Pro 不支持 /responses
	if c := classifyModelForResponses("DeepSeek-V4-Pro"); !c.IsAdapter() {
		t.Errorf("DeepSeek-V4-Pro 应走适配，got %+v", c)
	}
}

func TestResponsesToMessagesRequestBasic(t *testing.T) {
	doc := map[string]any{
		"model":             "claude sonnet 5",
		"instructions":      "be brief",
		"max_output_tokens": float64(1024),
		"temperature":       float64(0.5),
		"stream":            true,
		"tool_choice":       "auto",
		"input": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"type": "reasoning", "encrypted_content": "gAAAAsecret"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hi there"}}},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "read_file", "description": "read", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "web_search"}, // 非 function → 丢弃
		},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out["model"] != "claude sonnet 5" {
		t.Errorf("model = %v", out["model"])
	}
	if out["system"] != "be brief" {
		t.Errorf("system = %v, want instructions text", out["system"])
	}
	if out["max_tokens"] != 1024 {
		t.Errorf("max_tokens = %v, want 1024", out["max_tokens"])
	}
	if out["stream"] != true {
		t.Errorf("stream = %v, want true", out["stream"])
	}
	tc, _ := out["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "auto" {
		t.Errorf("tool_choice = %v, want {type:auto}", out["tool_choice"])
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 function tool", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "read_file" || tool["input_schema"] == nil {
		t.Errorf("tool = %v, want name+input_schema", tool)
	}
	// reasoning/encrypted_content 项被丢弃；user + assistant 保留
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (state-only dropped)", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "user" {
		t.Errorf("messages[0].role = %v", m0["role"])
	}
	c0 := m0["content"].([]any)[0].(map[string]any)
	if c0["type"] != "text" || c0["text"] != "hello" {
		t.Errorf("messages[0].content[0] = %v", c0)
	}
	// 确认加密态没有泄漏进 Messages 请求
	if strings.Contains(jsonStringify(out), "gAAAAsecret") {
		t.Error("encrypted_content leaked into Messages request")
	}
}

func TestResponsesToMessagesDefaultMaxTokens(t *testing.T) {
	doc := map[string]any{"model": "glm-4.7", "input": "hi"}
	out, err := responsesToMessagesRequest(doc, adapterOptions{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out["max_tokens"] != defaultMessagesMaxTokens {
		t.Errorf("max_tokens = %v, want default %d", out["max_tokens"], defaultMessagesMaxTokens)
	}
	// 字符串 input → 单条 user 消息
	msgs := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("string input = %v, want single user message", msgs)
	}
}

func TestResponsesToMessagesToolPairing(t *testing.T) {
	doc := map[string]any{
		"model": "claude sonnet 5",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"a.go"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "file contents"},
			map[string]any{"role": "user", "content": "thanks"},
		},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	// tool_use 在前，配对的 tool_result 紧跟其后
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "assistant" {
		t.Errorf("messages[0].role = %v, want assistant (tool_use)", m0["role"])
	}
	tu := m0["content"].([]any)[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "read_file" {
		t.Errorf("tool_use block = %v", tu)
	}
	if input, ok := tu["input"].(map[string]any); !ok || input["path"] != "a.go" {
		t.Errorf("tool_use.input = %v, want parsed {path:a.go}", tu["input"])
	}
	m1 := msgs[1].(map[string]any)
	tr := m1["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" || tr["content"] != "file contents" {
		t.Errorf("tool_result block = %v", tr)
	}
}

func TestResponsesToMessagesOrphanToolResult(t *testing.T) {
	// 没有配对 function_call 的 output → 降级为普通文本，避免上游报 orphan tool_result
	doc := map[string]any{
		"model": "claude sonnet 5",
		"input": []any{map[string]any{"type": "function_call_output", "call_id": "orphan_1", "output": "stale"}},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	msg := out["messages"].([]any)[0].(map[string]any)
	block := msg["content"].([]any)[0].(map[string]any)
	if block["type"] != "text" || !strings.Contains(stringifyAny(block["text"]), "Tool result orphan_1") {
		t.Errorf("orphan output = %v, want text fallback", block)
	}
}

func TestResponsesToMessagesKeepsToolStructureForTextOnlyModels(t *testing.T) {
	// 只支持文本的模型（原先会把历史工具调用压平成提示文本）同样走结构化 tool_use/tool_result：
	// 压平让多轮工具回合退化成文本转录，模型容易重复调用或跑偏
	doc := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"a.go"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "contents"},
		},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: false})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	tu := msgs[0].(map[string]any)
	if tu["role"] != "assistant" {
		t.Errorf("messages[0].role = %v, want assistant", tu["role"])
	}
	if b := tu["content"].([]any)[0].(map[string]any); b["type"] != "tool_use" || b["id"] != "call_1" {
		t.Errorf("messages[0] block = %v, want tool_use(call_1)", b)
	}
	tr := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" {
		t.Errorf("messages[1] block = %v, want 配对 tool_result", tr)
	}
	// 不应再出现压平后的中文提示文本
	if strings.Contains(jsonStringify(out), "历史工具") {
		t.Errorf("仍在压平历史工具: %s", jsonStringify(out))
	}
}

func TestNeutralizeLegacyToolCallTextUngated(t *testing.T) {
	// 旧版文本转录的中性化与模型能力无关：assistant 文本命中该格式就处理。
	// 用 codex 真实的 assistant 历史形状（output_text 块）。
	doc := map[string]any{
		"model": "claude sonnet 5",
		"input": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "Tool call call_9 (read_file):\nresult"},
			}},
			map[string]any{"role": "user", "content": "继续"},
		},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	first := out["messages"].([]any)[0].(map[string]any)
	text := stringifyAny(first["content"].([]any)[0].(map[string]any)["text"])
	if !strings.Contains(text, "历史工具调用文本记录") {
		t.Errorf("legacy 转录未中性化: %q", text)
	}
	// 不命中该格式的 assistant 文本原样保留为 assistant 消息
	plain := map[string]any{
		"model": "claude sonnet 5",
		"input": []any{map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "just a normal reply"},
		}}},
	}
	out2, err := responsesToMessagesRequest(plain, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert plain: %v", err)
	}
	m0 := out2["messages"].([]any)[0].(map[string]any)
	if m0["role"] != "assistant" {
		t.Errorf("普通 assistant 文本 role = %v, want assistant", m0["role"])
	}
}

func TestConvertContentBlockImages(t *testing.T) {
	dataURL := "data:image/png;base64,aGVsbG8="
	// 支持图片 + 当前输入 → 真正的 image 块
	block, err := convertContentBlock(map[string]any{"type": "input_image", "image_url": dataURL}, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert image: %v", err)
	}
	if block["type"] != "image" {
		t.Errorf("block = %v, want image", block)
	}
	src := block["source"].(map[string]any)
	if src["media_type"] != "image/png" || src["data"] != "aGVsbG8=" {
		t.Errorf("image source = %v", src)
	}
	// 历史图片 → 占位文本
	block, _ = convertContentBlock(map[string]any{"type": "input_image", "image_url": dataURL}, adapterOptions{SupportsImages: true, OmitImages: true})
	if block["type"] != "text" || block["text"] != historicalImageText {
		t.Errorf("historical image = %v, want placeholder", block)
	}
	// 模型不支持图片 → 占位文本
	block, _ = convertContentBlock(map[string]any{"type": "input_image", "image_url": dataURL}, adapterOptions{SupportsImages: false})
	if block["type"] != "text" || block["text"] != omittedImageText {
		t.Errorf("unsupported image = %v, want placeholder", block)
	}
	// 远程 URL → 报错
	if _, err := convertContentBlock(map[string]any{"type": "input_image", "image_url": "https://example.com/a.png"}, adapterOptions{SupportsImages: true}); err == nil {
		t.Error("remote image URL should error")
	}
	// 超大图片 → 占位文本（构造 > 5MB 的 base64）
	huge := strings.Repeat("A", 7*1024*1024)
	block, _ = convertContentBlock(map[string]any{
		"type": "input_image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": huge},
	}, adapterOptions{SupportsImages: true})
	if block["type"] != "text" || block["text"] != oversizedImageText {
		t.Errorf("oversized image = %.60v, want placeholder", block)
	}
}

func TestApplyToolChoice(t *testing.T) {
	tools := []any{map[string]any{"name": "read_file"}}
	newOut := func() map[string]any { return map[string]any{"tools": tools} }

	// auto
	out := newOut()
	applyToolChoice(out, "auto")
	if tc, _ := out["tool_choice"].(map[string]any); tc == nil || tc["type"] != "auto" {
		t.Errorf(`"auto" = %v, want {type:auto}`, out["tool_choice"])
	}
	// required / any → Messages 的 any
	for _, v := range []string{"required", "any"} {
		out = newOut()
		applyToolChoice(out, v)
		if tc, _ := out["tool_choice"].(map[string]any); tc == nil || tc["type"] != "any" {
			t.Errorf("%q = %v, want {type:any}", v, out["tool_choice"])
		}
	}
	// none → 去掉 tools（等价禁止调用，且不依赖上游是否认 {"type":"none"}）
	out = newOut()
	applyToolChoice(out, "none")
	if _, has := out["tools"]; has {
		t.Errorf(`"none" left tools in place: %v`, out)
	}
	if _, has := out["tool_choice"]; has {
		t.Errorf(`"none" should not emit tool_choice: %v`, out["tool_choice"])
	}
	// 指定函数 → {type:tool,name}
	out = newOut()
	applyToolChoice(out, map[string]any{"type": "function", "name": "read_file"})
	tc, _ := out["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "tool" || tc["name"] != "read_file" {
		t.Errorf("forced function = %v, want {type:tool,name:read_file}", out["tool_choice"])
	}
	// 嵌套 function.name 形式
	out = newOut()
	applyToolChoice(out, map[string]any{"type": "function", "function": map[string]any{"name": "grep"}})
	tc, _ = out["tool_choice"].(map[string]any)
	if tc == nil || tc["name"] != "grep" {
		t.Errorf("nested function name = %v", out["tool_choice"])
	}
	// 未知取值：不动 tools、不发 tool_choice
	out = newOut()
	applyToolChoice(out, "something_new")
	if _, has := out["tool_choice"]; has {
		t.Errorf("unknown value emitted tool_choice: %v", out["tool_choice"])
	}
	if _, has := out["tools"]; !has {
		t.Error("unknown value dropped tools")
	}
	// nil：什么都不做
	out = newOut()
	applyToolChoice(out, nil)
	if _, has := out["tool_choice"]; has {
		t.Errorf("nil emitted tool_choice: %v", out["tool_choice"])
	}
}

func TestResponsesToMessagesToolChoiceNoneDropsTools(t *testing.T) {
	doc := map[string]any{
		"model":       "claude sonnet 5",
		"input":       []any{map[string]any{"role": "user", "content": "hi"}},
		"tool_choice": "none",
		"tools":       []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object"}}},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, has := out["tools"]; has {
		t.Errorf("tool_choice=none should drop tools, got %v", out["tools"])
	}
}

func TestConvertContentBlockToolResult(t *testing.T) {
	// 字符串 content 原样
	block, err := convertContentBlock(map[string]any{
		"type": "tool_result", "tool_use_id": "call_1", "content": "plain output",
	}, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("string content: %v", err)
	}
	if block["content"] != "plain output" || block["tool_use_id"] != "call_1" {
		t.Errorf("string tool_result = %v", block)
	}
	// 数组 content 必须逐块转换后带上，不能字符串化成调试字面量
	block, err = convertContentBlock(map[string]any{
		"type": "tool_result", "tool_use_id": "call_2",
		"content": []any{map[string]any{"type": "text", "text": "hi"}},
	}, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("array content: %v", err)
	}
	blocks, ok := block["content"].([]any)
	if !ok {
		t.Fatalf("array content became %T (%v), want []any", block["content"], block["content"])
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %v, want 1", blocks)
	}
	inner := blocks[0].(map[string]any)
	if inner["type"] != "text" || inner["text"] != "hi" {
		t.Errorf("converted inner block = %v", inner)
	}
	// 回归：不应出现 Go map 渲染字面量
	if strings.Contains(jsonStringify(block), "map[") {
		t.Errorf("tool_result content stringified as Go literal: %s", jsonStringify(block))
	}
	// call_id / id 作为 tool_use_id 的回退
	block, _ = convertContentBlock(map[string]any{"type": "tool_result", "call_id": "call_3", "content": "x"}, adapterOptions{})
	if block["tool_use_id"] != "call_3" {
		t.Errorf("call_id fallback = %v", block)
	}
	// 缺 id → 报错
	if _, err := convertContentBlock(map[string]any{"type": "tool_result", "content": "x"}, adapterOptions{}); err == nil {
		t.Error("missing tool_use_id should error")
	}
}

func TestMessagesToResponsesBodyText(t *testing.T) {
	doc := map[string]any{
		"id": "msg_abc", "model": "claude sonnet 5", "role": "assistant", "stop_reason": "end_turn",
		"content": []any{map[string]any{"type": "text", "text": "hello world"}},
		"usage":   map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)},
	}
	out, err := messagesToResponsesBody(doc, 1700000000000, 1700000000, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out["id"] != "resp_msg_abc" {
		t.Errorf("id = %v, want resp_ prefix", out["id"])
	}
	if out["object"] != "response" || out["status"] != "completed" {
		t.Errorf("object/status = %v/%v", out["object"], out["status"])
	}
	output := out["output"].([]any)
	item := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("output item = %v", item)
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "hello world" {
		t.Errorf("output content = %v", content)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 5 || usage["total_tokens"] != 15 {
		t.Errorf("usage = %v", usage)
	}
}

func TestMessagesToResponsesBodyToolUse(t *testing.T) {
	doc := map[string]any{
		"id": "msg_1", "model": "claude sonnet 5",
		"content": []any{map[string]any{"type": "tool_use", "id": "toolu_9", "name": "read_file", "input": map[string]any{"path": "a.go"}}},
	}
	out, err := messagesToResponsesBody(doc, 1700000000000, 1700000000, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	item := out["output"].([]any)[0].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "read_file" {
		t.Errorf("output item = %v, want function_call", item)
	}
	if item["id"] != "fc_toolu_9" || item["call_id"] != "toolu_9" {
		t.Errorf("ids = %v/%v, want fc_toolu_9/toolu_9", item["id"], item["call_id"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(stringifyAny(item["arguments"])), &args); err != nil || args["path"] != "a.go" {
		t.Errorf("arguments = %v", item["arguments"])
	}
}

func TestNeutralizeLegacyToolCallTextMatchesWhatItReplaces(t *testing.T) {
	// A4 回归：括号前**没有空格**的写法，此前检测正则命中、替换正则不命中，
	// 结果只加了个外壳、原格式原样留着（等于没中性化）
	got := neutralizeLegacyToolCallText("Tool call call_9(read_file):\nresult")
	if got == "" {
		t.Fatal("无空格写法未被识别")
	}
	if strings.Contains(got, "Tool call call_9") {
		t.Errorf("加了外壳但没真替换: %q", got)
	}
	if !strings.Contains(got, "历史工具调用 read_file：") {
		t.Errorf("替换结果不含中性化文本: %q", got)
	}
	// 有空格的写法同样处理
	if g := neutralizeLegacyToolCallText("Tool call call_9 (read_file):\nx"); !strings.Contains(g, "历史工具调用 read_file：") {
		t.Errorf("有空格写法未替换: %q", g)
	}
	// 结果格式一并中性化
	if g := neutralizeLegacyToolCallText("Tool call call_1 (a):\nTool result call_1:\nout"); strings.Contains(g, "Tool result call_1:") {
		t.Errorf("Tool result 未中性化: %q", g)
	}
	// 不命中的文本返回空串，调用方保持原文（不加外壳）
	for _, s := range []string{"just a reply", "Tool calling something", "call_9 (x):"} {
		if g := neutralizeLegacyToolCallText(s); g != "" {
			t.Errorf("不该命中 %q，却返回 %q", s, g)
		}
	}
}

func TestNeutralizeOnlyRewritesMatchingBlocks(t *testing.T) {
	// A9 回归：此前命中就把整条 assistant 消息替换成单个 user 文本块 ——
	// 角色被翻成 user、其余内容块全被吞掉。现在只改命中的块，角色与其余块保持不变。
	doc := map[string]any{
		"model": "claude sonnet 5",
		"input": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "Tool call call_7 (read_file):\ndone"},
				map[string]any{"type": "output_text", "text": "另外补充一句正常回复"},
			}},
			map[string]any{"role": "user", "content": "继续"},
		},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	first := out["messages"].([]any)[0].(map[string]any)
	// 角色保持 assistant：翻成 user 会让模型把自己上一轮的输出当成用户输入
	if first["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", first["role"])
	}
	blocks := first["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content 块数 = %d, want 2（其余块不该被吞）: %s", len(blocks), jsonStringify(first))
	}
	b0 := stringifyAny(blocks[0].(map[string]any)["text"])
	if !strings.Contains(b0, "历史工具调用文本记录") || strings.Contains(b0, "Tool call call_7") {
		t.Errorf("命中块未正确中性化: %q", b0)
	}
	b1 := stringifyAny(blocks[1].(map[string]any)["text"])
	if b1 != "另外补充一句正常回复" {
		t.Errorf("未命中块被改动: %q", b1)
	}
	if strings.Contains(b1, "历史工具调用文本记录") {
		t.Errorf("外壳被加到了未命中的块上: %q", b1)
	}
}

func TestSanitizeAssistantText(t *testing.T) {
	if got := sanitizeAssistantText("hello<]minimax[>["); got != "hello" {
		t.Errorf("sanitize = %q, want hello", got)
	}
	if got := sanitizeAssistantText("<think>思考</think>正文"); got != "正文" {
		t.Errorf("sanitize = %q, want 正文", got)
	}
	if got := sanitizeAssistantText("</think>正文"); got != "正文" {
		t.Errorf("sanitize = %q, want 正文", got)
	}
	// <thinking> 变体与带空白的闭标签同样要剥干净（历史 bug：只认 <think> 字面量，
	// <thinking> 整块穿透进正文，被 codex 当文本渲染）。
	if got := sanitizeAssistantText("<thinking>思考</thinking>正文"); got != "正文" {
		t.Errorf("sanitize = %q, want 正文", got)
	}
	if got := sanitizeAssistantText("答案</think >"); got != "答案" {
		t.Errorf("sanitize = %q, want 答案", got)
	}
	if got := sanitizeAssistantText("plain text"); got != "plain text" {
		t.Errorf("sanitize changed clean text: %q", got)
	}
}

// TestStreamSafeSplitThinkVariants 验证跨 chunk 边界时 think 标签前缀会被缓冲住：
// 半个标签若当正文下发，客户端会实时渲染出原始标记。
// 回归：<thinking 前缀不在 marker 列表内，"<thinki" 这类尾部不被缓冲而直接下发。
func TestStreamSafeSplitThinkVariants(t *testing.T) {
	cases := []struct{ name, in, wantSafe, wantHold string }{
		{"thinking open split", "text<thinki", "text", "<thinki"},
		{"thinking close split", "a</thinkin", "a", "</thinkin"},
		// 闭标签恰好切成完整标签名（缺 >）：不能放行，否则 "</thinking" 原样下发且清洗剥不掉。
		{"thinking close exact split", "a</thinking", "a", "</thinking"},
		// 完整的闭标签不该被缓冲：等长不命中，交给 sanitize 剥除。
		{"closed thinking close tag", "a</thinking>", "a</thinking>", ""},
		{"bare think open split", "x<think", "x", "<think"},
		{"unclosed thinking block", "<thinking>abc", "", "<thinking>abc"},
		{"closed thinking block", "<thinking>abc</thinking>done", "<thinking>abc</thinking>done", ""},
		{"no marker", "plain", "plain", ""},
	}
	for _, c := range cases {
		safe, hold := streamSafeSplit(c.in)
		if safe != c.wantSafe || hold != c.wantHold {
			t.Errorf("%s: streamSafeSplit(%q) = (%q,%q), want (%q,%q)",
				c.name, c.in, safe, hold, c.wantSafe, c.wantHold)
		}
	}
}

// codexApplyPatchTool 复刻真实抓包里 codex 的 freeform 工具定义形状
// （catalog 里所有模型 apply_patch_tool_type=freeform，codex 就发这个）。
func codexApplyPatchTool() map[string]any {
	return map[string]any{
		"type": "custom", "name": "apply_patch",
		"description": "The `apply_patch` tool can be used to edit files. This is a FREEFORM tool, so do not wrap the patch in JSON.",
		"format": map[string]any{
			"type": "grammar", "syntax": "lark",
			"definition": "start: begin_patch hunk+ end_patch\nbegin_patch: \"*** Begin Patch\" LF",
		},
	}
}

func TestConvertCustomToolDefinition(t *testing.T) {
	tool := convertToolDefinition(codexApplyPatchTool())
	if tool == nil {
		t.Fatal("custom(freeform) 工具被丢弃了，模型将不知道 apply_patch 存在")
	}
	if tool["name"] != "apply_patch" {
		t.Errorf("name = %v", tool["name"])
	}
	// 单字符串参数：Messages 没有 grammar 约束工具，只能包一层
	schema := tool["input_schema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	input, ok := props[customToolInputKey].(map[string]any)
	if !ok || input["type"] != "string" {
		t.Fatalf("input_schema.properties.input = %v, want string 参数", props[customToolInputKey])
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != customToolInputKey {
		t.Errorf("required = %v, want [input]", schema["required"])
	}
	// lark 语法必须随 description 带给模型，否则它不知道 patch 格式
	desc := stringifyAny(tool["description"])
	if !strings.Contains(desc, "Begin Patch") || !strings.Contains(desc, "lark") {
		t.Errorf("description 未嵌入原始工具定义: %q", desc)
	}
}

func TestConvertToolsKeepsCodexToolset(t *testing.T) {
	// 真实抓包的工具集合：function 保留、custom 保留、托管工具丢弃
	tools := []any{
		map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
		codexApplyPatchTool(),
		map[string]any{"type": "tool_search", "execution": "client", "description": "..."},
		map[string]any{"type": "web_search", "external_web_access": false},
	}
	got := convertTools(tools)
	names := map[string]bool{}
	for _, tl := range got {
		names[stringifyAny(tl.(map[string]any)["name"])] = true
	}
	if !names["exec_command"] {
		t.Error("exec_command 丢了，执行命令能力会失效")
	}
	if !names["apply_patch"] {
		t.Error("apply_patch 丢了，写文件能力会失效")
	}
	if len(got) != 2 {
		t.Errorf("tools = %d 条 (%v)，want 2（托管工具 tool_search/web_search 应丢弃）", len(got), names)
	}
}

func TestCollectCustomToolNames(t *testing.T) {
	got := collectCustomToolNames([]any{
		map[string]any{"type": "function", "name": "exec_command"},
		codexApplyPatchTool(),
	})
	if !got["apply_patch"] || got["exec_command"] {
		t.Errorf("customTools = %v, want 只含 apply_patch", got)
	}
	if collectCustomToolNames(nil) != nil {
		t.Error("无 tools 时应返回 nil")
	}
}

func TestResponsesToMessagesCustomToolCallPairing(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch"
	doc := map[string]any{
		"model": "claude sonnet 5",
		"tools": []any{codexApplyPatchTool()},
		"input": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "ctc_1", "name": "apply_patch", "input": patch},
			map[string]any{"type": "custom_tool_call_output", "call_id": "ctc_1", "output": "Success"},
			map[string]any{"role": "user", "content": "继续"},
		},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	// custom_tool_call → tool_use，入参按单字符串参数包装
	tu := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "apply_patch" || tu["id"] != "ctc_1" {
		t.Fatalf("tool_use = %v", tu)
	}
	if in, _ := tu["input"].(map[string]any); in == nil || in[customToolInputKey] != patch {
		t.Errorf("tool_use.input = %v, want {input:<patch>}", tu["input"])
	}
	// custom_tool_call_output → 配对的 tool_result，而不是降级成纯文本
	tr := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "ctc_1" || tr["content"] != "Success" {
		t.Errorf("tool_result = %v, want 与 ctc_1 配对", tr)
	}
}

func TestResponsesToMessagesCustomToolChoice(t *testing.T) {
	out := map[string]any{"tools": []any{map[string]any{"name": "apply_patch"}}}
	applyToolChoice(out, map[string]any{"type": "custom", "name": "apply_patch"})
	tc, _ := out["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "tool" || tc["name"] != "apply_patch" {
		t.Errorf("tool_choice = %v, want {type:tool,name:apply_patch}", out["tool_choice"])
	}
}

func TestMessagesToResponsesBodyCustomToolCall(t *testing.T) {
	patch := "*** Begin Patch\n*** End Patch"
	doc := map[string]any{
		"id": "msg_1", "model": "claude sonnet 5",
		"content": []any{map[string]any{
			"type": "tool_use", "id": "toolu_7", "name": "apply_patch",
			"input": map[string]any{customToolInputKey: patch},
		}},
	}
	out, err := messagesToResponsesBody(doc, 1700000000000, 1700000000, map[string]bool{"apply_patch": true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	item := out["output"].([]any)[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("output item = %v, want custom_tool_call（发成 function_call 时 codex 会当格式错误）", item)
	}
	if item["input"] != patch {
		t.Errorf("input = %v, want 拆包后的纯文本 patch", item["input"])
	}
	if item["call_id"] != "toolu_7" || item["name"] != "apply_patch" {
		t.Errorf("call_id/name = %v/%v", item["call_id"], item["name"])
	}
	// 不在 customTools 名单里的仍按 function_call 走
	out2, _ := messagesToResponsesBody(doc, 1700000000000, 1700000000, nil)
	if it := out2["output"].([]any)[0].(map[string]any); it["type"] != "function_call" {
		t.Errorf("未声明 custom 的工具 = %v, want function_call", it["type"])
	}
}

func TestCustomToolInputTextFallback(t *testing.T) {
	// 模型没照单参数格式产出时不能丢内容
	if got := customToolInputText(map[string]any{"path": "a.go"}); !strings.Contains(got, "a.go") {
		t.Errorf("非包装对象 = %q, want 保留原内容", got)
	}
	if got := customToolInputText("raw text"); got != "raw text" {
		t.Errorf("字符串入参 = %q", got)
	}
}

func TestResponsesToMessagesMaxOutputTokensZeroUsesDefault(t *testing.T) {
	// G 项：max_output_tokens=0 原样透传 → 上游 400；应回退默认值
	doc := map[string]any{"model": "claude sonnet 5", "max_output_tokens": float64(0),
		"input": []any{map[string]any{"role": "user", "content": "hi"}}}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out["max_tokens"] != defaultMessagesMaxTokens {
		t.Errorf("max_tokens = %v, want default %d（0 应回退默认）", out["max_tokens"], defaultMessagesMaxTokens)
	}
}

func TestResponsesToMessagesInstructionsArray(t *testing.T) {
	// F4 回归：instructions 数组形式不能被丢掉
	doc := map[string]any{
		"model": "claude sonnet 5",
		"instructions": []any{
			map[string]any{"type": "text", "text": "be concise"},
		},
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if sys := stringifyAny(out["system"]); sys != "be concise" {
		t.Errorf("system = %q, want 'be concise'（数组 instructions 不能丢）", sys)
	}
}

func TestResponsesToMessagesStreamStringIsTreatedAsTrue(t *testing.T) {
	// G 项：stream:"true" 字符串原先被 bool 断言当 false，请求会静默变成非流式
	doc := map[string]any{
		"model":  "claude sonnet 5",
		"stream": "true",
		"input":  []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := responsesToMessagesRequest(doc, adapterOptions{SupportsImages: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out["stream"] != true {
		t.Errorf("stream = %v, want true（字符串 'true' 应视为开启流式）", out["stream"])
	}

	doc2 := map[string]any{"model": "claude sonnet 5", "stream": false,
		"input": []any{map[string]any{"role": "user", "content": "hi"}}}
	out2, _ := responsesToMessagesRequest(doc2, adapterOptions{SupportsImages: true})
	if out2["stream"] != false {
		t.Errorf("stream=false 应保持 false")
	}
}

func TestMessagesToResponsesBodyToolUseWithText(t *testing.T) {
	// G 项：convertOutputItems 有 tool_use 时丢掉同消息的文字内容（模型同时返回思考文字+工具调用时信息丢失）
	doc := map[string]any{
		"id": "msg_1", "model": "claude sonnet 5", "role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "I'll read the file"},
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": map[string]any{"path": "a.go"}},
		},
		"usage": map[string]any{"input_tokens": float64(5), "output_tokens": float64(10)},
	}
	out, err := messagesToResponsesBody(doc, 1700000000000, 1700000000, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	items := out["output"].([]any)
	if len(items) < 2 {
		t.Fatalf("output = %d 项，应有工具调用 + 文本（共 2 项）", len(items))
	}
	hasFC, hasMsg := false, false
	for _, item := range items {
		m := item.(map[string]any)
		if m["type"] == "function_call" {
			hasFC = true
		}
		if m["type"] == "message" {
			hasMsg = true
		}
	}
	if !hasFC || !hasMsg {
		t.Errorf("output = %v, want function_call + message", items)
	}
}

func TestNormalizeExecCommandArgs(t *testing.T) {
	// command → cmd 归一
	got := normalizeExecCommandArgs("exec_command", `{"command":"ls -la","timeout":5000}`)
	if !strings.Contains(got, `"cmd":"ls -la"`) || strings.Contains(got, `"command"`) {
		t.Errorf("command→cmd 未归一: %s", got)
	}
	if !strings.Contains(got, `"yield_time_ms":5000`) || strings.Contains(got, `"timeout"`) {
		t.Errorf("timeout→yield_time_ms 未归一: %s", got)
	}

	// timeout 钳制到上限 30000
	got = normalizeExecCommandArgs("exec_command", `{"command":"x","timeout":60000}`)
	if !strings.Contains(got, `"yield_time_ms":30000`) {
		t.Errorf("timeout 上限钳制失败: %s", got)
	}
	// timeout 钳制到下限 250
	got = normalizeExecCommandArgs("exec_command", `{"command":"x","timeout":10}`)
	if !strings.Contains(got, `"yield_time_ms":250`) {
		t.Errorf("timeout 下限钳制失败: %s", got)
	}

	// 非 exec_command 不处理
	got = normalizeExecCommandArgs("read_file", `{"command":"x","timeout":5000}`)
	if !strings.Contains(got, `"command"`) {
		t.Errorf("非 exec_command 不应归一: %s", got)
	}

	// 非法 JSON 原样返回
	got = normalizeExecCommandArgs("exec_command", `not-json`)
	if got != "not-json" {
		t.Errorf("非法 JSON 应原样返回: %s", got)
	}

	// 已有 cmd/yield_time_ms 不重复处理
	got = normalizeExecCommandArgs("exec_command", `{"cmd":"ls","yield_time_ms":3000}`)
	if got != `{"cmd":"ls","yield_time_ms":3000}` {
		t.Errorf("已有归一字段不应改动: %s", got)
	}
}
