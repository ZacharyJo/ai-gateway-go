package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testHeadroomConfig 构造测试用配置：阈值放低，便于命中接受。
func testHeadroomConfig() HeadroomConfig {
	return HeadroomConfig{
		Mode: "on", Apply: true,
		MinChars: 512, HeadChars: 2500, TailChars: 2500, MaxJSONItems: 15,
		KeepLineContext: 3, MaxSnippets: 20,
		MinSavedChars: 100, MinSavingsRatio: 0.2, MinSavedTokens: 0, MinTokenSavingsRatio: 0,
		LiveZonePolicy: "latest-per-type",
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"hello", 2},       // 5 字母 → ceil(5/4)
		{"world hello", 4}, // world=2 + hello=2
		{"中文测试", 4},        // 4 个单字
		{"123456", 2},      // 6 数字 → ceil(6/3)
		{"!!!", 3},         // 3 个标点
		{"abc123", 2},      // abc=1 + 123=1
		{"a b c", 3},       // 3 个单字母（空白不计）
	}
	for _, c := range cases {
		if got := estimateTokens(c.text); got != c.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestTryCompressJsonTable(t *testing.T) {
	// 20+ 条同质对象 → json_table
	items := make([]any, 0, 22)
	for i := 0; i < 22; i++ {
		items = append(items, map[string]any{"name": "file" + itoa(i), "status": "ok", "count": float64(i)})
	}
	text := jsonStringify(items)
	c := tryCompressJsonText(text, 15, "hash123")
	if c == nil {
		t.Fatal("json_table candidate = nil")
	}
	if c.Kind != "json_table" {
		t.Fatalf("kind = %s, want json_table", c.Kind)
	}
	if !strings.Contains(c.Compressed, "kind=json_table") {
		t.Errorf("missing kind header: %q", c.Compressed)
	}
	if !strings.Contains(c.Compressed, "[22]{") {
		t.Errorf("missing schema line: %q", c.Compressed)
	}
	// 列按频次降序、同频按名称升序（三列都出现 22 次 → count,name,status）
	if !strings.Contains(c.Compressed, "[22]{count:number,name:string,status:string}") {
		t.Errorf("missing schema columns: %q", c.Compressed)
	}
}

func TestTryCompressJsonArraySampling(t *testing.T) {
	// 非对象数组 → json_array 采样（前4+后3+重要行）
	items := make([]any, 0, 30)
	for i := 0; i < 30; i++ {
		items = append(items, "item-"+itoa(i))
	}
	// 一个"重要"项（error 关键词）
	items[20] = "fatal error happened"
	text := jsonStringify(items)
	c := tryCompressJsonText(text, 15, "hash123")
	if c == nil {
		t.Fatal("json_array candidate = nil")
	}
	if c.Kind != "json_array" {
		t.Fatalf("kind = %s, want json_array", c.Kind)
	}
	if !strings.Contains(c.Compressed, "kind=json_array") || !strings.Contains(c.Compressed, "fatal error happened") {
		t.Errorf("json_array output missing key parts: %q", c.Compressed)
	}
}

func TestTryCompressJsonMinify(t *testing.T) {
	// 无数组可压 → minify（lossless）
	text := `{  "a" : 1,   "b" : [1,2,3]  }`
	c := tryCompressJsonText(text, 15, "hash123")
	if c == nil {
		t.Fatal("minify candidate = nil")
	}
	if c.Kind != "json_minified" || !c.Lossless {
		t.Fatalf("kind = %s lossless = %v, want json_minified/true", c.Kind, c.Lossless)
	}
	if len(c.Compressed) >= len(text) {
		t.Errorf("minify not smaller: %d >= %d", len(c.Compressed), len(text))
	}
}

func TestValidateCompressionChain(t *testing.T) {
	// 宽松基础配置，按用例覆盖单个闸门
	loose := &HeadroomConfig{MinSavedChars: 100, MinSavingsRatio: 0, MinSavedTokens: 50, MinTokenSavingsRatio: 0.05}
	long := strings.Repeat("a", 1000) // 250 tokens
	cases := []struct {
		name string
		comp *Compression
		cfg  *HeadroomConfig
		want string
	}{
		{"not smaller", &Compression{OriginalChars: 100, OriginalText: long, Compressed: strings.Repeat("b", 100)}, loose, "rejected_not_smaller"},
		{"below min saved chars", &Compression{OriginalChars: 1000, OriginalText: long, Compressed: strings.Repeat("b", 950)}, loose, "below_min_saved_chars"},
		{"below savings ratio", &Compression{OriginalChars: 1000, OriginalText: long, Compressed: strings.Repeat("b", 850)}, &HeadroomConfig{MinSavedChars: 100, MinSavingsRatio: 0.5, MinSavedTokens: 0, MinTokenSavingsRatio: 0}, "below_min_savings_ratio"},
		{"lossless accepted", &Compression{OriginalChars: 1000, OriginalText: long, Compressed: strings.Repeat("b", 100), Lossless: true}, loose, "accepted_lossless"},
		// 压缩后字节更短但 token 更多（400 个中文字符 = 400 tokens > 250）→ rejected_not_fewer_tokens
		{"not fewer tokens", &Compression{OriginalChars: 1000, OriginalText: long, Compressed: strings.Repeat("中", 400)}, loose, "rejected_not_fewer_tokens"},
		// "hello world"*25 = 100 tokens；"hi world"*20 = 60 tokens；savedTokens=40 < 50
		{"below min saved tokens", &Compression{OriginalChars: 275, OriginalText: strings.Repeat("hello world", 25), Compressed: strings.Repeat("hi world", 20)}, loose, "below_min_saved_tokens"},
		// tokenRatio=0.4 < 0.5
		{"below token ratio", &Compression{OriginalChars: 275, OriginalText: strings.Repeat("hello world", 25), Compressed: strings.Repeat("hi world", 20)}, &HeadroomConfig{MinSavedChars: 100, MinSavingsRatio: 0, MinSavedTokens: 0, MinTokenSavingsRatio: 0.5}, "below_min_token_savings_ratio"},
		{"accepted", &Compression{OriginalChars: 1000, OriginalText: long, Compressed: "x"}, loose, "accepted"},
	}
	for _, c := range cases {
		if got := validateCompression(c.comp, c.cfg).reason; got != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCollectCandidatesLatestPerType(t *testing.T) {
	h := newHeadroom(testHeadroomConfig())
	doc := map[string]any{
		"input": []any{
			map[string]any{"type": "function_call_output", "output": strings.Repeat("x", 600)},
			map[string]any{"type": "message", "role": "assistant", "content": "no"},
			map[string]any{"type": "function_call_output", "output": strings.Repeat("y", 600)},
			map[string]any{"type": "local_shell_call_output", "output": strings.Repeat("z", 600)},
		},
	}
	eligible, cands := h.collectCandidates("/v1/responses", doc)
	if !eligible {
		t.Fatal("not eligible")
	}
	// latest-per-type：每种 type 只留最后一条 → function_call_output(index2) + local_shell_call_output(index3)
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2 (latest per type)", len(cands))
	}
	if cands[0].index != 2 || cands[1].index != 3 {
		t.Errorf("candidate indexes = %d,%d, want 2,3", cands[0].index, cands[1].index)
	}
	// 非 /v1/responses 不参与
	if _, cands := h.collectCandidates("/v1/chat/completions", doc); len(cands) != 0 {
		t.Error("chat/completions should not be eligible")
	}
}

func TestCompressGitDiff(t *testing.T) {
	diff := `diff --git a/foo.txt b/foo.txt
index 111..222 100644
--- a/foo.txt
+++ b/foo.txt
@@ -1,5 +1,6 @@
 context line one
 context line two
-old line
+new line one
+new line two
 context line three
 context line four
\ No newline at end of file`
	lines := splitLines(diff)
	if !isGitDiff(lines) {
		t.Fatal("not detected as git diff")
	}
	h := newHeadroom(testHeadroomConfig())
	c := h.compressLargeText(diff)
	if c.Kind != "git_diff" {
		t.Fatalf("kind = %s, want git_diff", c.Kind)
	}
	if !strings.Contains(c.Compressed, "diff --git a/foo.txt") || !strings.Contains(c.Compressed, "new line one") || !strings.Contains(c.Compressed, "\\ No newline") {
		t.Errorf("git_diff missing preserved lines: %q", c.Compressed)
	}
}

func TestCompressLogOrMultiline(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("some ordinary log line text here with number 42\n")
	}
	sb.WriteString("ERROR: fatal failure happened\n")
	text := sb.String()
	h := newHeadroom(testHeadroomConfig())
	c := h.compressLargeText(text)
	if c.Kind != "log_or_multiline" {
		t.Fatalf("kind = %s, want log_or_multiline", c.Kind)
	}
	if !strings.Contains(c.Compressed, "--- head ---") || !strings.Contains(c.Compressed, "--- tail ---") {
		t.Errorf("missing head/tail sections: %q", c.Compressed)
	}
	if len(c.Compressed) >= len(text) {
		t.Errorf("log compression not smaller: %d >= %d", len(c.Compressed), len(text))
	}
}

func TestCompressSearchResults(t *testing.T) {
	var lines []string
	for f := 0; f < 3; f++ {
		for n := 1; n <= 5; n++ {
			lines = append(lines, "src/file"+itoa(f)+".go:"+itoa(n)+":func doSomething"+itoa(f)+itoa(n)+"() error {")
		}
	}
	text := strings.Join(lines, "\n")
	h := newHeadroom(testHeadroomConfig())
	c := h.compressLargeText(text)
	if c.Kind != "search_results" {
		t.Fatalf("kind = %s, want search_results", c.Kind)
	}
	if !strings.Contains(c.Compressed, "more matches in") && !strings.Contains(c.Compressed, "kind=search_results") {
		t.Errorf("search_results output missing markers: %q", c.Compressed)
	}
}

func TestShouldUseCandidateCharLen(t *testing.T) {
	cfg := testHeadroomConfig()
	cfg.MinChars = 600
	h := newHeadroom(cfg)
	// 500 个中文 = 1500 字节但只有 500 字符 → 不该入选（与 Node 的 .length 口径一致）
	cjk := strings.Repeat("中", 500)
	if h.shouldUseCandidate(map[string]any{"type": "function_call_output", "output": cjk}) {
		t.Errorf("500 CJK chars (%d bytes) should not pass MinChars=600 gate", len(cjk))
	}
	// 700 个中文 → 入选
	if !h.shouldUseCandidate(map[string]any{"type": "function_call_output", "output": strings.Repeat("中", 700)}) {
		t.Error("700 CJK chars should pass MinChars=600 gate")
	}
	// 类型不在允许集 → 不入选
	if h.shouldUseCandidate(map[string]any{"type": "message", "output": strings.Repeat("a", 1000)}) {
		t.Error("message type should never be a candidate")
	}
	// output 非字符串 → 不入选
	if h.shouldUseCandidate(map[string]any{"type": "function_call_output", "output": []any{"a"}}) {
		t.Error("non-string output should not be a candidate")
	}
}

func TestShouldUseCandidateCodexCustomToolOutput(t *testing.T) {
	// 官方 Codex 的工具输出是 custom_tool_call_output（shell/文件读取等大输出都走它）。
	// 回归：加进认可名单后应入选；仍要求 output 是字符串。
	cfg := testHeadroomConfig()
	cfg.MinChars = 10
	h := newHeadroom(cfg)
	if !h.shouldUseCandidate(map[string]any{"type": "custom_tool_call_output", "output": strings.Repeat("x", 50)}) {
		t.Error("custom_tool_call_output with string output should be a candidate (Codex native type)")
	}
	// 结构化 output（content 数组）→ 不入选（headroom 只压纯字符串）
	if h.shouldUseCandidate(map[string]any{"type": "custom_tool_call_output", "output": []any{"a"}}) {
		t.Error("non-string custom_tool_call_output should not be a candidate")
	}
}

func TestCompressLargeTextDeterministic(t *testing.T) {
	// 同一输入必须产出完全相同的压缩结果：selectLogLines 的上下文扩展曾在遍历 map 时插入，
	// 导致结果随机（Go 规范：迭代中新增的键可能被访问也可能被跳过），破坏上游 prompt cache。
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		sb.WriteString("ordinary log line with value 7\n")
	}
	sb.WriteString("ERROR: first failure at stage one\n")
	for i := 0; i < 30; i++ {
		sb.WriteString("more ordinary log output here\n")
	}
	sb.WriteString("WARNING: something odd\n")
	sb.WriteString("ERROR: second failure at stage two\n")
	for i := 0; i < 30; i++ {
		sb.WriteString("trailing log line\n")
	}
	text := sb.String()

	h := newHeadroom(testHeadroomConfig())
	first := h.compressLargeText(text)
	if first.Kind != "log_or_multiline" {
		t.Fatalf("kind = %s, want log_or_multiline", first.Kind)
	}
	for i := 0; i < 30; i++ {
		got := h.compressLargeText(text)
		if got.Compressed != first.Compressed {
			t.Fatalf("compression not deterministic on run %d:\nfirst len=%d\ngot   len=%d",
				i+2, len(first.Compressed), len(got.Compressed))
		}
	}
}

func TestApplyToRawBody(t *testing.T) {
	storeDir := t.TempDir()
	cfg := testHeadroomConfig()
	cfg.StoreDir = storeDir
	h := newHeadroom(cfg)

	big := strings.Repeat("some ordinary log line text here\n", 200)
	doc := map[string]any{
		"model": "gpt-5.6",
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": big},
			map[string]any{"type": "message", "role": "assistant", "content": "ok"},
		},
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	res := h.applyToRawBody("/v1/responses", body, parsed)
	if !res.Changed {
		t.Fatal("expected changed=true in on mode")
	}
	if !res.RawRewrite {
		t.Error("expected raw rewrite (byte-preserving)")
	}
	// 改写后的 body 仍是合法 JSON，且 input[0].output 被压缩、input[1] 原样
	var out map[string]any
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("rewritten body invalid JSON: %v\n%s", err, res.Body)
	}
	items := out["input"].([]any)
	item0 := items[0].(map[string]any)
	item1 := items[1].(map[string]any)
	if !strings.HasPrefix(item0["output"].(string), "[ai-gateway headroom:") {
		t.Errorf("output[0] not compressed: %.60s", item0["output"])
	}
	if item1["content"] != "ok" {
		t.Errorf("output[1] should be untouched: %v", item1["content"])
	}
	// raw 改写应保留非改写字节（缩进、assistant 消息原样）
	if !strings.Contains(string(res.Body), "\"type\": \"message\"") {
		t.Error("raw rewrite lost original formatting")
	}
	// 原文落盘（applyToRawBody 只返回 originals，落盘由 process/writeOriginals 负责）
	h.writeOriginals(res.Originals)
	if len(res.Originals) == 0 {
		t.Fatal("no originals recorded")
	}
	hash := res.Originals[0].Hash
	if _, err := os.Stat(filepath.Join(storeDir, hash+".txt")); err != nil {
		t.Errorf("original not written to store: %v", err)
	}
	if got, ok := h.readOriginal(hash); !ok || got != big {
		t.Error("readOriginal mismatch")
	}
}

func TestHeadroomProcessDryRunNoChange(t *testing.T) {
	cfg := testHeadroomConfig()
	cfg.Mode = "dry-run"
	cfg.Apply = false
	h := newHeadroom(cfg)
	big := strings.Repeat("some ordinary log line text here\n", 200)
	doc := map[string]any{"input": []any{map[string]any{"type": "function_call_output", "output": big}}}
	body, _ := json.Marshal(doc)
	log := NewLogger(nil, true)
	got := h.process("/v1/responses", body, log, 1)
	if string(got) != string(body) {
		t.Error("dry-run should not modify body")
	}
}

func TestHeadroomProcessOff(t *testing.T) {
	cfg := testHeadroomConfig()
	cfg.Mode = "off"
	h := newHeadroom(cfg)
	body := []byte(`{"input":[]}`)
	log := NewLogger(nil, true)
	if got := h.process("/v1/responses", body, log, 1); string(got) != string(body) {
		t.Error("off mode should pass through")
	}
}

func TestRawRewriteLocate(t *testing.T) {
	body := `{"input":[
  {"type":"function_call_output","output":"abc"},
  {"type":"function_call_output","output":"def"}
]}`
	start, end, val, ok := findInputOutputStringRange([]byte(body), 1)
	if !ok || val != "def" {
		t.Fatalf("locate index 1 = (%d,%d,%q,%v), want def", start, end, val, ok)
	}
	if body[start] != '"' || body[end-1] != '"' {
		t.Errorf("range not a string literal: [%d,%d) = %q", start, end, body[start:end])
	}
	// 改写 index 1，其余字节不变
	stat := &CandidateStat{Path: "$.input[1].output"}
	out := rewriteAcceptedOutputsRaw([]byte(body), []*CandidateStat{stat}, map[string]string{stat.Path: "compressed-value"})
	got := string(out)
	if !strings.Contains(got, `"output":"compressed-value"`) {
		t.Errorf("rewrite failed: %s", got)
	}
	if !strings.Contains(got, `"output":"abc"`) {
		t.Errorf("unrelated bytes changed: %s", got)
	}
}

func TestRewriteAcceptedOutputsOverlap(t *testing.T) {
	body := `{"input":[
  {"type":"function_call_output","output":"abc"},
  {"type":"function_call_output","output":"def"}
]}`
	// 两个替换区间不应重叠（这里各自独立，应成功）
	stats := []*CandidateStat{{Path: "$.input[0].output"}, {Path: "$.input[1].output"}}
	reps := map[string]string{"$.input[0].output": "AAA", "$.input[1].output": "BBB"}
	if out := rewriteAcceptedOutputsRaw([]byte(body), stats, reps); out == nil {
		t.Fatal("non-overlapping rewrite returned nil")
	} else if !strings.Contains(string(out), `"output":"BBB"`) {
		t.Errorf("second replacement missing: %s", out)
	}
}

// TestSliceTailCharsUTF16 验证 sliceChars/tailChars 按 UTF-16 码元计数（与 JS slice 对齐），
// 且不会切出悬空代理（输出恒为有效 UTF-8，charLen(结果) ≤ n）。
func TestSliceTailCharsUTF16(t *testing.T) {
	// 增补字符 😀 占 2 个 UTF-16 码元；a/b 各占 1。
	s := "a😀b"

	// sliceChars
	if got := sliceChars(s, 1); got != "a" {
		t.Errorf(`sliceChars(s,1) = %q, want "a"`, got)
	}
	if got := sliceChars(s, 2); got != "a" {
		t.Errorf(`sliceChars(s,2) = %q, want "a"（😀 为 2 码元，越界整字符跳过）`, got)
	}
	if got := sliceChars(s, 3); got != "a😀" {
		t.Errorf(`sliceChars(s,3) = %q, want "a😀"`, got)
	}
	if got := sliceChars(s, 10); got != s {
		t.Errorf(`sliceChars(s,10) = %q, want %q`, got, s)
	}
	if charLen(sliceChars(s, 2)) > 2 {
		t.Error("sliceChars 结果 charLen 不应超过 n")
	}

	// tailChars
	if got := tailChars(s, 1); got != "b" {
		t.Errorf(`tailChars(s,1) = %q, want "b"`, got)
	}
	if got := tailChars(s, 2); got != "b" {
		t.Errorf(`tailChars(s,2) = %q, want "b"`, got)
	}
	if got := tailChars(s, 3); got != "😀b" {
		t.Errorf(`tailChars(s,3) = %q, want "😀b"`, got)
	}
	if got := tailChars(s, 10); got != s {
		t.Errorf(`tailChars(s,10) = %q, want %q`, got, s)
	}
	if charLen(tailChars(s, 2)) > 2 {
		t.Error("tailChars 结果 charLen 不应超过 n")
	}

	// 纯 ASCII 行为与旧实现一致
	ascii := "abcdef"
	if got := sliceChars(ascii, 3); got != "abc" {
		t.Errorf(`sliceChars(ascii,3) = %q`, got)
	}
	if got := tailChars(ascii, 3); got != "def" {
		t.Errorf(`tailChars(ascii,3) = %q`, got)
	}
	if got := sliceChars(ascii, 0); got != "" {
		t.Errorf(`sliceChars(ascii,0) = %q, want ""`, got)
	}
}
