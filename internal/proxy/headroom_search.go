package proxy

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 搜索结果压缩（对应 ai-gateway headroom/search_compressor.mjs）。

// searchMatch 是一条解析出的 "file:line:content" 匹配。
type searchMatch struct {
	file       string
	lineNumber int
	separator  string
	content    string
	original   string
}

// parseSearchLine 解析单行 file:line:content（对应 parseSearchLine）。
// 支持 Windows 盘符前缀（C:/...、C:\...）；分隔符为 : 或 -。
func parseSearchLine(line string) *searchMatch {
	scanStart := 0
	if len(line) >= 3 && isASCIILetter(line[0]) && line[1] == ':' && (line[2] == '/' || line[2] == '\\') {
		scanStart = 2
	}
	for i := scanStart; i < len(line); i++ {
		sep := line[i]
		if sep != ':' && sep != '-' {
			continue
		}
		if i > 0 && (line[i-1] == ':' || line[i-1] == '-') {
			continue
		}
		j := i + 1
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j == i+1 || j >= len(line) || (line[j] != ':' && line[j] != '-') {
			continue
		}
		if i == 0 {
			return nil
		}
		num, err := strconv.Atoi(line[i+1 : j])
		if err != nil {
			return nil
		}
		return &searchMatch{file: line[:i], lineNumber: num, separator: string(sep), content: line[j+1:], original: line}
	}
	return nil
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// parseSearchResults 解析多行搜索结果为 文件 → 匹配列表，返回命中数。
func parseSearchResults(lines []string) (map[string][]*searchMatch, int) {
	files := map[string][]*searchMatch{}
	parsed := 0
	for _, line := range lines {
		match := parseSearchLine(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		parsed++
		files[match.file] = append(files[match.file], match)
	}
	return files, parsed
}

// isSearchResults 判断是否为搜索结果输出（对应 isSearchResults）：
// 行数 >= 10、解析命中 >= 10、命中率 >= 0.7、至少一个文件。
func isSearchResults(lines []string) bool {
	if len(lines) < 10 {
		return false
	}
	files, parsed := parseSearchResults(lines)
	return parsed >= 10 && float64(parsed)/float64(len(lines)) >= 0.7 && len(files) > 0
}

var reSearchSensitive = regexp.MustCompile(`(?i)\b(?:todo|fixme|security|auth|token|secret|password|credential)\b`)
var reSearchCode = regexp.MustCompile(`(?i)\b(?:function|class|export|import|const|let|var|def|struct|impl)\b`)

// scoreSearchMatch 给搜索结果打分（对应 scoreSearchMatch）。
func scoreSearchMatch(match *searchMatch) float64 {
	score := 0.0
	if lineScore(match.content) >= 0.8 {
		score += 0.5
	}
	if reSearchSensitive.MatchString(match.content) {
		score += 0.4
	}
	if reSearchCode.MatchString(match.content) {
		score += 0.1
	}
	return math.Min(1, score)
}

// selectSearchMatches 按文件总分数排序选出高价值匹配（对应 selectSearchMatches）。
// 每个文件最多 maxMatchesPerFile 条，总计最多 maxTotalMatches 条；每文件必留首条+末条+高分条。
func selectSearchMatches(files map[string][]*searchMatch, maxFiles, maxMatchesPerFile, maxTotalMatches int) map[string][]*searchMatch {
	type scoredFile struct {
		file    string
		matches []*searchMatch
	}
	scored := make([]scoredFile, 0, len(files))
	for file, matches := range files {
		sc := scoredFile{file: file}
		for _, m := range matches {
			// 每项复制 score（Node 展开新对象）
			sc.matches = append(sc.matches, &searchMatch{file: m.file, lineNumber: m.lineNumber, separator: m.separator, content: m.content, original: m.original})
		}
		scored = append(scored, sc)
	}
	// 按文件总分数降序、文件名字典序升序
	sort.SliceStable(scored, func(i, j int) bool {
		si := sumScore(scored[i].matches)
		sj := sumScore(scored[j].matches)
		if si != sj {
			return si > sj
		}
		return scored[i].file < scored[j].file
	})
	if len(scored) > maxFiles {
		scored = scored[:maxFiles]
	}
	selected := map[string][]*searchMatch{}
	total := 0
	for _, group := range scored {
		if total >= maxTotalMatches {
			break
		}
		cap := min(maxMatchesPerFile, maxTotalMatches-total)
		chosen := make([]*searchMatch, 0, cap)
		seen := map[string]bool{}
		add := func(m *searchMatch) {
			if m == nil || len(chosen) >= cap {
				return
			}
			key := strconv.Itoa(m.lineNumber) + ":" + m.content
			if seen[key] {
				return
			}
			seen[key] = true
			chosen = append(chosen, m)
		}
		add(group.matches[0])
		if len(group.matches) > 1 {
			add(group.matches[len(group.matches)-1])
		}
		sorted := append([]*searchMatch(nil), group.matches...)
		sort.SliceStable(sorted, func(a, b int) bool {
			if scoreSearchMatch(sorted[a]) != scoreSearchMatch(sorted[b]) {
				return scoreSearchMatch(sorted[a]) > scoreSearchMatch(sorted[b])
			}
			return sorted[a].lineNumber < sorted[b].lineNumber
		})
		for _, m := range sorted {
			add(m)
		}
		sort.SliceStable(chosen, func(a, b int) bool { return chosen[a].lineNumber < chosen[b].lineNumber })
		selected[group.file] = chosen
		total += len(chosen)
	}
	return selected
}

func sumScore(matches []*searchMatch) float64 {
	sum := 0.0
	for _, m := range matches {
		sum += scoreSearchMatch(m)
	}
	return sum
}

// formatSearchResults 格式化选中结果（对应 formatSearchResults），
// 每文件末尾附 "… and N more matches in <file>" 提示。
func formatSearchResults(files map[string][]*searchMatch, selected map[string][]*searchMatch) string {
	keys := make([]string, 0, len(selected))
	for k := range selected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, file := range keys {
		for _, m := range selected[file] {
			out = append(out, m.file+m.separator+strconv.Itoa(m.lineNumber)+m.separator+m.content)
		}
		originalCount := len(files[file])
		if originalCount > len(selected[file]) {
			out = append(out, "[... and "+strconv.Itoa(originalCount-len(selected[file]))+" more matches in "+file+"]")
		}
	}
	return strings.Join(out, "\n")
}
