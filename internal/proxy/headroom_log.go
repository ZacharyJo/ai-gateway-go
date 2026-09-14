package proxy

import (
	"regexp"
	"sort"
	"strings"
)

// 日志行精选（对应 ai-gateway headroom/log_compressor.mjs）。

var reErrorLevel = regexp.MustCompile(`(?i)npm ERR!|\b(?:error|exception|fatal|critical|panic|segfault|assertionerror|traceback)\b`)
var reFailLevel = regexp.MustCompile(`(?i)\b(?:fail|failed|denied|timeout|timed out|cannot|unable)\b`)
var reWarnLevel = regexp.MustCompile(`(?i)\b(?:warn|warning)\b`)
var reInfoLevel = regexp.MustCompile(`(?i)\b(?:info|notice)\b`)
var reDebugLevel = regexp.MustCompile(`(?i)\bdebug\b`)
var reTraceLevel = regexp.MustCompile(`(?i)\btrace\b`)

// classifyLogLevel 分级日志级别（对应 classifyLogLevel）。
func classifyLogLevel(line string) string {
	switch {
	case reErrorLevel.MatchString(line):
		return "error"
	case reFailLevel.MatchString(line):
		return "fail"
	case reWarnLevel.MatchString(line):
		return "warn"
	case reInfoLevel.MatchString(line):
		return "info"
	case reDebugLevel.MatchString(line):
		return "debug"
	case reTraceLevel.MatchString(line):
		return "trace"
	default:
		return "unknown"
	}
}

var reStackFile = regexp.MustCompile(`^File ".+", line \d+`)
var reStackAtIndented = regexp.MustCompile(`^\s+at [\w.$<>]+`)
var reStackAt = regexp.MustCompile(`^at [\w.$<>]+`)
var reStackCausedBy = regexp.MustCompile(`^\s*Caused by:`)
var reStackDash = regexp.MustCompile(`^\s*---`)
var reStackErrorType = regexp.MustCompile(`^\s*(?:AssertionError|TypeError|ReferenceError|SyntaxError|RuntimeError|ValueError|Error):`)
var reStackCall = regexp.MustCompile(`^\s*\w+(?:\.\w+)*\([^)]*:\d+(?::\d+)?\)`)

// isStackTraceLine 判断是否为栈帧行（对应 isStackTraceLine，注意各正则作用的输入不同）。
func isStackTraceLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(trimmed, "Traceback (most recent call last)") {
		return true
	}
	return reStackFile.MatchString(trimmed) ||
		reStackAtIndented.MatchString(line) ||
		reStackAt.MatchString(trimmed) ||
		reStackCausedBy.MatchString(line) ||
		reStackDash.MatchString(line) ||
		reStackErrorType.MatchString(trimmed) ||
		reStackCall.MatchString(trimmed)
}

var reSummary = regexp.MustCompile(`(?i)(?:short test summary|test suites:|^\s*tests?:|^\s*(?:passed|failed|skipped|collected)\b|npm ERR!|make: \*\*\*)`)

// isSummaryLine 判断是否为总结行（对应 isSummaryLine）。
func isSummaryLine(line string) bool {
	return reSummary.MatchString(line)
}

var levelScores = map[string]float64{
	"error": 1, "fail": 1, "warn": 0.5, "info": 0.1, "debug": 0.05, "trace": 0.02, "unknown": 0.1,
}

// lineScore 计算日志行重要性分数（0~1，对应 lineScore）。
func lineScore(line string) float64 {
	score := levelScores[classifyLogLevel(line)]
	if isStackTraceLine(line) {
		score += 0.3
	}
	if isSummaryLine(line) {
		score += 0.4
	}
	return min(1, score)
}

// dedupeKey 日志行去重键（对应 dedupeLogLines 的 key）：
// 截断到首个 :/=、0x 十六进制 → 0xN、数字 → N。
func dedupeKey(line string) string {
	key := strings.TrimSpace(line)
	if m := reDedupeCut.FindStringSubmatch(key); m != nil {
		key = m[1]
	}
	key = reDedupeHex.ReplaceAllString(key, "0xN")
	key = reDedupeDigits.ReplaceAllString(key, "N")
	return key
}

var reDedupeCut = regexp.MustCompile(`^(.*?[:=]).*$`)
var reDedupeHex = regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b`)
var reDedupeDigits = regexp.MustCompile(`\b\d+\b`)

// logEntry 是 selectLogLines 内部的一条候选行。
type logEntry struct {
	line    string
	index   int
	level   string
	stack   bool
	summary bool
	score   float64
}

// dedupeLogLines 按 dedupeKey 去重（对应 dedupeLogLines）。
func dedupeLogLines(entries []*logEntry) []*logEntry {
	seen := map[string]bool{}
	out := make([]*logEntry, 0, len(entries))
	for _, e := range entries {
		key := dedupeKey(e.line)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// selectLogLines 精选日志行（对应 selectLogLines）：
// 首个+末个错误 + 高分行 + 去重警告 + 栈帧 + 总结，再补 ±contextLines 上下文，按行号排序截断。
func selectLogLines(lines []string, contextLines, maxSnippets int) []string {
	entries := make([]*logEntry, len(lines))
	for i, line := range lines {
		entries[i] = &logEntry{line: line, index: i, level: classifyLogLevel(line), stack: isStackTraceLine(line), summary: isSummaryLine(line), score: lineScore(line)}
	}
	selected := map[int]*logEntry{}
	add := func(e *logEntry) {
		if e != nil {
			selected[e.index] = e
		}
	}
	var errors, highScore, stack, summaries []*logEntry
	var warnings []*logEntry
	for _, e := range entries {
		if e.level == "error" || e.level == "fail" {
			errors = append(errors, e)
		}
		if e.level == "warn" {
			warnings = append(warnings, e)
		}
		if e.stack {
			stack = append(stack, e)
		}
		if e.summary {
			summaries = append(summaries, e)
		}
		if e.score >= 0.8 {
			highScore = append(highScore, e)
		}
	}
	warnings = dedupeLogLines(warnings)
	if len(warnings) > 5 {
		warnings = warnings[:5]
	}
	if len(stack) > max(5, maxSnippets) {
		stack = stack[:max(5, maxSnippets)]
	}
	sort.SliceStable(highScore, func(i, j int) bool {
		if highScore[i].score != highScore[j].score {
			return highScore[i].score > highScore[j].score
		}
		return highScore[i].index < highScore[j].index
	})
	if len(highScore) > max(10, maxSnippets) {
		highScore = highScore[:max(10, maxSnippets)]
	}
	if len(errors) > 0 {
		add(errors[0])
		add(errors[len(errors)-1])
	}
	for _, e := range highScore {
		add(e)
	}
	for _, e := range warnings {
		add(e)
	}
	for _, e := range stack {
		add(e)
	}
	for _, e := range summaries {
		add(e)
	}
	// 上下文扩展：先快照当前选中项的下标再扩展。
	// 不能直接 range selected 边遍历边 add —— Go 规范规定迭代中新增的键"可能被访问也可能被跳过"，
	// 会让同一份输入产出不同结果，破坏压缩产物的确定性（进而破坏上游 prompt cache 命中）。
	seeds := make([]int, 0, len(selected))
	for idx := range selected {
		seeds = append(seeds, idx)
	}
	sort.Ints(seeds)
	for _, seedIdx := range seeds {
		start := max(0, seedIdx-contextLines)
		end := min(len(lines)-1, seedIdx+contextLines)
		for idx := start; idx <= end; idx++ {
			add(entries[idx])
		}
	}
	// 按行号排序，截断
	ordered := make([]*logEntry, 0, len(selected))
	for _, e := range selected {
		ordered = append(ordered, e)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	limit := max(20, maxSnippets*(contextLines*2+1))
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	out := make([]string, 0, len(ordered))
	for _, e := range ordered {
		out = append(out, e.line)
	}
	return out
}

// uniqueLines 按 trim 后的内容去重并限制数量（对应 headroom.uniqueLines）。
func uniqueLines(lines []string, limit int) []string {
	out := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, line := range lines {
		normalized := strings.TrimSpace(line)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, line)
		if len(out) >= limit {
			break
		}
	}
	return out
}
