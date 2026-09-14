package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Headroom：上下文压缩（Lite 版）。
// 三态：off（不启用）/ dry-run（只分析不改写）/ on（真正改写）。
// 只处理 /v1/responses 的 input 数组里 type ∈ {function_call_output, local_shell_call_output,
// apply_patch_call_output} 且 output 长度 >= minChars 的项；改写用字节保真 raw 替换，
// 保证其余字节（缩进、encrypted_content、键序）不变以命中上游 prompt cache。

// APPROVED_RESPONSE_OUTPUT_TYPES 可压缩的 input 项类型。
// custom_tool_call_output 是官方 Codex 的原生工具输出类型（shell/文件读取等大输出都走它），
// 原实现主要服务 Claude Code/CodexOne（function_call_output 系）；
// 实机流量验证发现不加它 headroom 对 Codex 完全不触发，故补充（only 字符串 output 会被压）。
var approvedResponseOutputTypes = map[string]bool{
	"function_call_output":    true,
	"local_shell_call_output": true,
	"apply_patch_call_output": true,
	"custom_tool_call_output": true,
}

// HeadroomConfig 是 headroom 的运行配置（默认值内置）。
type HeadroomConfig struct {
	Mode                 string  // off / dry-run / on
	Apply                bool    // on 时 true
	MinChars             int     // 候选最小字符数（>=512）
	HeadChars            int     // head 截取字符（>=500）
	TailChars            int     // tail 截取字符（>=500）
	MaxJSONItems         int     // JSON 采样上限（>=5）
	StoreDir             string  // 原文存储目录
	KeepLineContext      int     // 日志/diff 上下文行数（>=1）
	MaxSnippets          int     // 片段上限（>=1）
	MinSavedChars        int     // 最小节省字符（>=100）
	MinSavingsRatio      float64 // 最小节省比例
	MinSavedTokens       int     // 最小节省 token
	MinTokenSavingsRatio float64 // 最小 token 节省比例
	LiveZoneItems        int     // 只压最后 N 条候选（0=不限）
	LiveZonePolicy       string  // latest-per-type / all
}

// Headroom 是压缩器实例。
type Headroom struct {
	cfg HeadroomConfig
}

// Analysis 是一次 analyzeRequest 的结果。
type Analysis struct {
	Mode       string
	Eligible   bool
	Candidates []*CandidateStat
	Accepted   []*CandidateStat
	Rejected   []*CandidateStat
}

// Original 是一条被压缩的原文（落盘供 /headroom-lite/<sha> 取回）。
type Original struct {
	Hash string
	Path string
	Text string
}

// ApplyResult 是 applyToRawBody 的结果。
type ApplyResult struct {
	Body       []byte
	Changed    bool
	Analysis   *Analysis
	Originals  []Original
	RawRewrite bool
}

// newHeadroom 构造压缩器。
func newHeadroom(cfg HeadroomConfig) *Headroom {
	return &Headroom{cfg: cfg}
}

// Enabled 返回是否启用（off 时 false）。
func (h *Headroom) Enabled() bool {
	return h.cfg.Mode != "off"
}

// compressLargeText 压缩大文本（对应 compressLargeText）：JSON → diff → 搜索 → 日志 → 兜底文本。
func (h *Headroom) compressLargeText(text string) *Compression {
	hash := hashText(text)
	chars := charLen(text)
	base := &Compression{OriginalChars: chars, OriginalText: text, Hash: hash, Kind: "text"}
	if c := tryCompressJsonText(text, h.cfg.MaxJSONItems, hash); c != nil {
		base.Kind = c.Kind
		base.Compressed = c.Compressed
		base.Lossless = c.Lossless
		return base
	}
	lines := splitLines(text)
	if isGitDiff(lines) {
		diff := compressGitDiff(lines, h.cfg.KeepLineContext)
		base.Kind = "git_diff"
		header := "[ai-gateway headroom: compressed tool output; kind=git_diff; original_lines=" + itoa(len(lines)) +
			"; kept_lines=" + itoa(diff.keptLines) + "; files=" + itoa(diff.files) + "; hunks=" + itoa(diff.hunks) +
			"; additions=" + itoa(diff.additions) + "; deletions=" + itoa(diff.deletions) +
			"; omitted_context_lines=" + itoa(diff.omittedContext) + "; original_chars=" + itoa(chars) + "; sha256=" + hash + "]"
		base.Compressed = header + "\n" + diff.text
		return base
	}
	if isSearchResults(lines) {
		files, parsed := parseSearchResults(lines)
		selected := selectSearchMatches(files, 15, 5, max(10, h.cfg.MaxSnippets))
		base.Kind = "search_results"
		kept := 0
		for _, v := range selected {
			kept += len(v)
		}
		header := "[ai-gateway headroom: compressed tool output; kind=search_results; original_matches=" + itoa(parsed) +
			"; kept_matches=" + itoa(kept) + "; files=" + itoa(len(files)) + "; original_chars=" + itoa(chars) + "; sha256=" + hash + "]"
		base.Compressed = header + "\n" + formatSearchResults(files, selected)
		return base
	}
	if len(lines) >= 40 {
		sel := uniqueLines(selectLogLines(lines, h.cfg.KeepLineContext, h.cfg.MaxSnippets),
			max(20, h.cfg.MaxSnippets*(h.cfg.KeepLineContext*2+1)))
		base.Kind = "log_or_multiline"
		header := "[ai-gateway headroom: compressed tool output; kind=log_or_multiline; original_lines=" + itoa(len(lines)) +
			"; kept_lines=" + itoa(len(sel)) + "; original_chars=" + itoa(chars) + "; sha256=" + hash + "]"
		parts := []string{header, "--- head ---", sliceChars(text, h.cfg.HeadChars)}
		if len(sel) > 0 {
			parts = append(parts, "--- important/context lines ---", strings.Join(sel, "\n"))
		}
		parts = append(parts, "--- tail ---", tailChars(text, h.cfg.TailChars))
		base.Compressed = strings.Join(parts, "\n")
		return base
	}
	base.Compressed = "[ai-gateway headroom: compressed tool output; kind=text; original_chars=" + itoa(chars) +
		"; sha256=" + hash + "]\n--- head ---\n" + sliceChars(text, h.cfg.HeadChars) +
		"\n--- tail ---\n" + tailChars(text, h.cfg.TailChars)
	return base
}

// compressTextFailOpen 压缩单个候选，失败返回 error（fail-open 第一层）。
func (h *Headroom) compressTextFailOpen(text string) (comp *Compression, err error) {
	defer func() {
		if r := recover(); r != nil {
			comp = nil
			err = fmt.Errorf("%v", r)
		}
	}()
	return h.compressLargeText(text), nil
}

// normalizedPath 归一化路径（对应 normalizedPath）：去 query/fragment、去尾部斜杠。
func normalizedPath(path string) string {
	path = strings.SplitN(path, "?", 2)[0]
	path = strings.SplitN(path, "#", 2)[0]
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	return path
}

// isEligibleRequest 判断请求是否可压缩：/v1/responses 且 json 对象且 input 为数组。
func (h *Headroom) isEligibleRequest(path string, jsonDoc any) bool {
	if !h.Enabled() {
		return false
	}
	doc, ok := jsonDoc.(map[string]any)
	if !ok {
		return false
	}
	_, ok = doc["input"].([]any)
	return normalizedPath(path) == "/v1/responses" && ok
}

// shouldUseCandidate 判断单个候选是否可用：类型在允许集、output 为字符串且长度达标。
func (h *Headroom) shouldUseCandidate(item map[string]any) bool {
	if item == nil {
		return false
	}
	if !approvedResponseOutputTypes[stringifyAny(item["type"])] {
		return false
	}
	output, ok := item["output"].(string)
	// 用 charLen（UTF-16 视角）而非 len（字节数）：与 Node 的 output.length 语义一致，
	// 否则中文工具输出会因字节数虚高被提前纳入候选，与判定链里的 charLen 口径不一致。
	return ok && charLen(output) >= h.cfg.MinChars
}

// collectCandidates 收集候选并应用 live-zone 策略（对应 collectCandidates）。
func (h *Headroom) collectCandidates(path string, jsonDoc any) (bool, []*candidate) {
	if !h.isEligibleRequest(path, jsonDoc) {
		return false, nil
	}
	doc := jsonDoc.(map[string]any)
	input, _ := doc["input"].([]any)
	var candidates []*candidate
	for i, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || !h.shouldUseCandidate(item) {
			continue
		}
		candidates = append(candidates, &candidate{item: item, index: i, path: "$.input[" + itoa(i) + "].output"})
	}
	if h.cfg.LiveZonePolicy == "latest-per-type" {
		latestByType := map[string]*candidate{}
		for _, c := range candidates {
			latestByType[stringifyAny(c.item["type"])] = c
		}
		candidates = candidates[:0]
		for _, c := range latestByType {
			candidates = append(candidates, c)
		}
		// 按原下标升序（map 遍历无序，必须显式排序保证候选顺序确定）
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].index < candidates[j].index })
	}
	if h.cfg.LiveZoneItems > 0 && len(candidates) > h.cfg.LiveZoneItems {
		candidates = candidates[len(candidates)-h.cfg.LiveZoneItems:]
	}
	return true, candidates
}

// analyzeRequest 分析候选（对应 analyzeRequest）：每个候选压缩 + 判定，分 accepted/rejected。
func (h *Headroom) analyzeRequest(path string, jsonDoc any) *Analysis {
	eligible, cands := h.collectCandidates(path, jsonDoc)
	analysis := &Analysis{Mode: h.cfg.Mode, Eligible: eligible}
	for _, cand := range cands {
		comp, err := h.compressTextFailOpen(cand.item["output"].(string))
		if err != nil {
			stat := buildFailureStat(cand, "compression_failed", err.Error())
			analysis.Candidates = append(analysis.Candidates, stat)
			analysis.Rejected = append(analysis.Rejected, stat)
			continue
		}
		decision := validateCompression(comp, &h.cfg)
		stat := buildCompressionStat(cand, comp, decision.accepted, decision.reason)
		if decision.accepted {
			stat.compressedText = comp.Compressed
			analysis.Accepted = append(analysis.Accepted, stat)
		} else {
			analysis.Rejected = append(analysis.Rejected, stat)
		}
		analysis.Candidates = append(analysis.Candidates, stat)
	}
	return analysis
}

// applyToRawBody 改写 body：先分析，on 模式且有接受候选时改写（raw 优先，失败整体重序列化）。
func (h *Headroom) applyToRawBody(path string, body []byte, jsonDoc any) *ApplyResult {
	analysis := h.analyzeRequest(path, jsonDoc)
	if !h.cfg.Apply || len(analysis.Accepted) == 0 {
		return &ApplyResult{Body: body, Analysis: analysis}
	}
	out := cloneJSON(jsonDoc)
	replacementsByPath := map[string]string{}
	originals := []Original{}
	outMap, _ := out.(map[string]any)
	for _, stat := range analysis.Accepted {
		index := parseInputIndex(stat.Path)
		if index < 0 || outMap == nil {
			continue
		}
		input, ok := outMap["input"].([]any)
		if !ok || index >= len(input) {
			continue
		}
		item, ok := input[index].(map[string]any)
		if !ok {
			continue
		}
		original, ok := item["output"].(string)
		if !ok {
			continue
		}
		var comp *Compression
		if stat.compressedText != "" {
			comp = &Compression{Hash: stat.Hash, Compressed: stat.compressedText}
		} else {
			comp = h.compressLargeText(original)
		}
		item["output"] = comp.Compressed
		replacementsByPath[stat.Path] = comp.Compressed
		originals = append(originals, Original{Hash: comp.Hash, Path: stat.Path, Text: original})
	}
	rawBody := rewriteAcceptedOutputsRaw(body, analysis.Accepted, replacementsByPath)
	result := &ApplyResult{Body: body, Changed: len(originals) > 0, Analysis: analysis, Originals: originals, RawRewrite: rawBody != nil}
	if rawBody != nil {
		result.Body = rawBody
	} else {
		if b, err := json.Marshal(outMap); err == nil {
			result.Body = b
		}
	}
	return result
}

// writeOriginals 落盘原文到 storeDir（写失败不影响请求，fail-open）。
// 原文是用户机器上的工具输出（文件内容/shell 输出），按 0600 落盘避免同机其他用户可读。
func (h *Headroom) writeOriginals(originals []Original) {
	if len(originals) == 0 || h.cfg.StoreDir == "" {
		return
	}
	if err := os.MkdirAll(h.cfg.StoreDir, 0o700); err != nil {
		return
	}
	for _, o := range originals {
		_ = os.WriteFile(filepath.Join(h.cfg.StoreDir, o.Hash+".txt"), []byte(o.Text), 0o600)
	}
}

// readOriginal 取回压缩前原文，hash 非法或不存在返回空。
func (h *Headroom) readOriginal(hash string) (string, bool) {
	if !validHashPattern.MatchString(hash) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(h.cfg.StoreDir, hash+".txt"))
	if err != nil {
		return "", false
	}
	return string(b), true
}

var validHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// process 是上游转发前的入口：解析 JSON → 分析 → on 模式改写 + 落盘原文。
// 返回改写后的 body（未启用/未命中时原样）。
func (h *Headroom) process(clientPath string, body []byte, log *Logger, reqID int64) []byte {
	if !h.Enabled() {
		return body
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	// 非 /v1/responses 或非候选场景（dry-run 也只记录命中）
	res := h.applyToRawBody(clientPath, body, doc)
	if res.Analysis != nil && len(res.Analysis.Candidates) > 0 {
		sum := summarizeStats(res.Analysis, h.cfg.Mode)
		log.Infof("headroom req=%d %s candidates=%d accepted=%d saved_chars=%d saved_tokens=%d",
			reqID, clientPath, sum.Candidates, sum.Accepted, sum.SavedChars, sum.SavedTokens)
	}
	if !res.Changed {
		return body
	}
	h.writeOriginals(res.Originals)
	return res.Body
}

// ---- 公共辅助 ----

// jsonStringify 序列化任意值为 JSON 字符串（对应 JSON.stringify）。
func jsonStringify(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// stringifyAny 把 any 转字符串（用于 type 等字段，非 JSON 序列化）。
func stringifyAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// isPlainObject 判断是否为 JSON 对象（非数组）。
func isPlainObject(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// cloneJSON 深拷贝（对应 cloneJson：JSON 往返）。
func cloneJSON(v any) any {
	var out any
	_ = json.Unmarshal([]byte(jsonStringify(v)), &out)
	return out
}

// hashText 返回 sha256 hex。
func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// splitLines 按 \r?\n 切分（对应 JS split(/\r?\n/)，结尾换行产生一个空串）。
var newlinePattern = regexp.MustCompile("\r?\n")

func splitLines(text string) []string {
	return newlinePattern.Split(text, -1)
}

// charLen 返回字符串的 UTF-16 码元数（与 JS String.length 对齐；BMP 字符计 1，增补字符计 2）。
// Headroom 的字符门槛/长度比较都基于此，不能用 Go len()（字节数）。
func charLen(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// sliceChars 取前 n 个 UTF-16 码元（对应 JS slice(0, n)；BMP 字符计 1、增补字符计 2）。
// 若切点落在代理对中间则整字符跳过，保证 charLen(结果) ≤ n 且输出是有效 UTF-8。
func sliceChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	units := 0
	for i, r := range runes {
		u := 1
		if r > 0xFFFF {
			u = 2
		}
		if units+u > n {
			return string(runes[:i])
		}
		units += u
	}
	return s
}

// tailChars 取最后 n 个 UTF-16 码元（对应 JS slice(-n)）。语义同 sliceChars。
func tailChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	units := 0
	start := len(runes)
	for i := len(runes) - 1; i >= 0; i-- {
		u := 1
		if runes[i] > 0xFFFF {
			u = 2
		}
		if units+u > n {
			break
		}
		units += u
		start = i
	}
	return string(runes[start:])
}

// indentJSON 2 空格缩进序列化（对应 JSON.stringify(v, null, 2)）。
func indentJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return jsonStringify(v)
	}
	return string(b)
}
