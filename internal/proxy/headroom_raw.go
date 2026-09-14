package proxy

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// raw 字节保真改写器（对应 ai-gateway headroom/raw_rewriter.mjs）。
// 在原始请求字节里精确定位 $.input[i].output 字符串字面量区间，做非重叠替换，
// 保证其余字节（缩进、encrypted_content、键序）逐字节不变，最大化上游 prompt cache 命中。
// 任何一处定位失败 → 返回 nil，调用方回退整体重新序列化。

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func skipJSONWhitespace(b []byte, pos int) int {
	for pos < len(b) && isJSONWhitespace(b[pos]) {
		pos++
	}
	return pos
}

// readJSONStringLiteral 从 b[pos]=='"' 开始读 JSON 字符串，返回 (原始片段, 结束下标, 解码值)。
func readJSONStringLiteral(b []byte, pos int) (raw string, end int, value string, ok bool) {
	if pos >= len(b) || b[pos] != '"' {
		return "", 0, "", false
	}
	escaped := false
	for p := pos + 1; p < len(b); p++ {
		ch := b[p]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			var val string
			if err := json.Unmarshal(b[pos:p+1], &val); err != nil {
				return "", 0, "", false
			}
			return string(b[pos : p+1]), p + 1, val, true
		}
	}
	return "", 0, "", false
}

// findMatchingDelimiter 找与 start 处 open 匹配的 close 下标，失败返回 -1。
func findMatchingDelimiter(b []byte, start int, open, close byte) int {
	depth := 0
	pos := start
	for pos < len(b) {
		if b[pos] == '"' {
			_, end, _, ok := readJSONStringLiteral(b, pos)
			if !ok {
				return -1
			}
			pos = end
			continue
		}
		if b[pos] == open {
			depth++
		}
		if b[pos] == close {
			depth--
			if depth == 0 {
				return pos
			}
		}
		pos++
	}
	return -1
}

// findObjectProperty 在 [objectStart, objectEnd) 对象里找 key 的值起点，失败返回 -1。
func findObjectProperty(b []byte, objectStart, objectEnd int, key string) int {
	pos := objectStart + 1
	for pos < objectEnd {
		pos = skipJSONWhitespace(b, pos)
		if pos >= objectEnd || b[pos] == '}' {
			return -1
		}
		_, litEnd, litValue, ok := readJSONStringLiteral(b, pos)
		if !ok {
			return -1
		}
		pos = skipJSONWhitespace(b, litEnd)
		if pos >= objectEnd || b[pos] != ':' {
			return -1
		}
		pos = skipJSONWhitespace(b, pos+1)
		valueStart := pos
		if litValue == key {
			return valueStart
		}
		// 跳过该值
		if pos < objectEnd && b[pos] == '{' {
			end := findMatchingDelimiter(b, pos, '{', '}')
			if end < 0 {
				return -1
			}
			pos = end + 1
		} else if pos < objectEnd && b[pos] == '[' {
			end := findMatchingDelimiter(b, pos, '[', ']')
			if end < 0 {
				return -1
			}
			pos = end + 1
		} else if pos < objectEnd && b[pos] == '"' {
			_, end, _, ok := readJSONStringLiteral(b, pos)
			if !ok {
				return -1
			}
			pos = end
		} else {
			for pos < objectEnd && b[pos] != ',' && b[pos] != '}' {
				pos++
			}
		}
		pos = skipJSONWhitespace(b, pos)
		if pos < objectEnd && b[pos] == ',' {
			pos++
		}
	}
	return -1
}

// findArrayElement 在 [arrayStart, arrayEnd) 数组里找第 targetIndex 个元素，返回 [valueStart, valueEnd)。
func findArrayElement(b []byte, arrayStart, arrayEnd, targetIndex int) (int, int) {
	pos := arrayStart + 1
	index := 0
	for pos < arrayEnd {
		pos = skipJSONWhitespace(b, pos)
		if pos >= arrayEnd || b[pos] == ']' {
			return -1, -1
		}
		valueStart := pos
		var valueEnd int
		switch {
		case b[pos] == '{':
			end := findMatchingDelimiter(b, pos, '{', '}')
			if end < 0 {
				return -1, -1
			}
			valueEnd = end + 1
		case b[pos] == '[':
			end := findMatchingDelimiter(b, pos, '[', ']')
			if end < 0 {
				return -1, -1
			}
			valueEnd = end + 1
		case b[pos] == '"':
			_, end, _, ok := readJSONStringLiteral(b, pos)
			if !ok {
				return -1, -1
			}
			valueEnd = end
		default:
			for pos < arrayEnd && b[pos] != ',' && b[pos] != ']' {
				pos++
			}
			valueEnd = pos
		}
		if index == targetIndex {
			return valueStart, valueEnd
		}
		pos = skipJSONWhitespace(b, valueEnd)
		if pos < arrayEnd && b[pos] == ',' {
			pos++
		}
		index++
	}
	return -1, -1
}

// findInputOutputStringRange 定位 $.input[inputIndex].output 字符串字面量区间 [start, end)（含引号）。
func findInputOutputStringRange(b []byte, inputIndex int) (start, end int, value string, ok bool) {
	rootStart := skipJSONWhitespace(b, 0)
	if rootStart >= len(b) || b[rootStart] != '{' {
		return 0, 0, "", false
	}
	rootEnd := findMatchingDelimiter(b, rootStart, '{', '}')
	if rootEnd < 0 {
		return 0, 0, "", false
	}
	input := findObjectProperty(b, rootStart, rootEnd, "input")
	if input < 0 || input >= len(b) || b[input] != '[' {
		return 0, 0, "", false
	}
	inputEnd := findMatchingDelimiter(b, input, '[', ']')
	if inputEnd < 0 {
		return 0, 0, "", false
	}
	itemStart, itemEnd := findArrayElement(b, input, inputEnd, inputIndex)
	if itemStart < 0 || itemStart >= len(b) || b[itemStart] != '{' {
		return 0, 0, "", false
	}
	output := findObjectProperty(b, itemStart, itemEnd-1, "output")
	if output < 0 || output >= len(b) || b[output] != '"' {
		return 0, 0, "", false
	}
	_, litEnd, litValue, ok := readJSONStringLiteral(b, output)
	if !ok {
		return 0, 0, "", false
	}
	return output, litEnd, litValue, true
}

var inputPathPattern = regexp.MustCompile(`^\$\.input\[(\d+)\]\.output$`)

// parseInputIndex 从候选路径 "$.input[3].output" 提取下标，非法返回 -1。
func parseInputIndex(path string) int {
	m := inputPathPattern.FindStringSubmatch(path)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

// rewriteAcceptedOutputsRaw 在原始字节上做非重叠区间替换；任一定位失败或区间重叠返回 nil。
func rewriteAcceptedOutputsRaw(body []byte, accepted []*CandidateStat, replacementsByPath map[string]string) []byte {
	text := string(body)
	type replacement struct {
		start, end int
		repl       string
	}
	var reps []replacement
	for _, stat := range accepted {
		index := parseInputIndex(stat.Path)
		repl, ok := replacementsByPath[stat.Path]
		if index < 0 || !ok {
			return nil
		}
		start, end, _, ok := findInputOutputStringRange([]byte(text), index)
		if !ok {
			return nil
		}
		reps = append(reps, replacement{start: start, end: end, repl: jsonStringify(repl)})
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].start < reps[j].start })
	for i := 1; i < len(reps); i++ {
		if reps[i].start < reps[i-1].end {
			return nil
		}
	}
	var sb strings.Builder
	last := 0
	for _, r := range reps {
		sb.WriteString(text[last:r.start])
		sb.WriteString(r.repl)
		last = r.end
	}
	sb.WriteString(text[last:])
	return []byte(sb.String())
}
