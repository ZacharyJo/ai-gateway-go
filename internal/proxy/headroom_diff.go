package proxy

import (
	"regexp"
	"strconv"
	"strings"
)

// git diff 压缩（对应 ai-gateway headroom/diff_compressor.mjs）。

var reGitDiffHeader = regexp.MustCompile(`^(diff --git|diff --combined |diff --cc |--- a\/|\+\+\+ b\/|@@\s+-\d+(?:,\d+)?\s+\+\d+(?:,\d+)?\s+@@|@@@+\s+-\d+)`)
var reGitHunkStart = regexp.MustCompile(`^@@@?\s`)
var reGitMetaLine = regexp.MustCompile(`^(index |new file mode |deleted file mode |similarity index |rename from |rename to |--- |\+\+\+ |Binary files )`)

// isGitDiff 判断是否为 git diff 输出（前 120 行内命中 diff 特征行）。
func isGitDiff(lines []string) bool {
	scan := lines[:min(len(lines), 120)]
	for _, line := range scan {
		if reGitDiffHeader.MatchString(line) {
			return true
		}
	}
	return false
}

// trimDiffHunk 裁剪 hunk：保留 +/- 行及其 ±contextLines 上下文（+/- 行本身不保留 +++/--- 元行）。
func trimDiffHunk(hunk []string, contextLines int) []string {
	keep := map[int]bool{}
	for idx, line := range hunk {
		if (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) &&
			!strings.HasPrefix(line, "+++") && !strings.HasPrefix(line, "---") {
			start := max(0, idx-contextLines)
			end := min(len(hunk)-1, idx+contextLines)
			for i := start; i <= end; i++ {
				keep[i] = true
			}
		}
		if strings.HasPrefix(line, "\\ No newline at end of file") {
			keep[idx] = true
		}
	}
	out := make([]string, 0, len(hunk))
	for idx, line := range hunk {
		if keep[idx] {
			out = append(out, line)
		}
	}
	return out
}

// diffStats 是 git diff 压缩的统计。
type diffStats struct {
	text           string
	files          int
	hunks          int
	keptLines      int
	additions      int
	deletions      int
	omittedContext int
}

// compressGitDiff 压缩 git diff（对应 compressGitDiff）：
// 保留文件头/元行/hunk 头，hunk 内只保留 +/- 行及上下文。
func compressGitDiff(lines []string, contextLines int) diffStats {
	var out []string
	var s diffStats
	var currentHunk []string
	inHunk := false

	flushHunk := func() {
		if !inHunk {
			return
		}
		trimmed := trimDiffHunk(currentHunk, contextLines)
		out = append(out, trimmed...)
		s.keptLines += len(trimmed)
		s.omittedContext += max(0, len(currentHunk)-len(trimmed))
		currentHunk = nil
		inHunk = false
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "diff --combined ") || strings.HasPrefix(line, "diff --cc ") {
			flushHunk()
			s.files++
			out = append(out, line)
			continue
		}
		if reGitHunkStart.MatchString(line) {
			flushHunk()
			s.hunks++
			out = append(out, line)
			inHunk = true
			currentHunk = nil
			continue
		}
		if inHunk {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				s.additions++
			}
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				s.deletions++
			}
			currentHunk = append(currentHunk, line)
			continue
		}
		if reGitMetaLine.MatchString(line) {
			out = append(out, line)
		}
	}
	flushHunk()
	s.text = strings.Join(out, "\n")
	return s
}

// 供 headroom.go 汇总用
func itoa(n int) string { return strconv.Itoa(n) }
