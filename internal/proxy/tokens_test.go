package proxy

import "testing"

// estimateTokens 的基础用例在 headroom_test.go 的 TestEstimateTokens；
// 这里只覆盖 A10 修的空白字符类（Go 的 \s 比 JS 窄）。

func TestEstimateTokensJSWhitespaceNotCounted(t *testing.T) {
	// A10：Go 的 \s 只含 ASCII [\t\n\f\r ]，JS 的 \s 还含全角空格等。
	// 少列一个，含全角空格的中文就会被当成"其他字符"多算一个 token，
	// headroom 的 MinSavedTokens / MinTokenSavingsRatio 阈值相对参考实现漂移。
	for _, ws := range []struct {
		name string
		s    string
	}{
		{"全角空格 U+3000", "　"},
		{"不换行空格 U+00A0", " "},
		{"垂直制表 U+000B", "\v"},
		{"OGHAM U+1680", " "},
		{"EN QUAD U+2000", " "},
		{"HAIR SPACE U+200A", " "},
		{"行分隔 U+2028", " "},
		{"段分隔 U+2029", " "},
		{"窄不换行空格 U+202F", " "},
		{"MEDIUM MATH U+205F", " "},
		{"BOM U+FEFF", "\ufeff"},
	} {
		// 单独一个空白字符：不该产生任何 token
		if got := estimateTokens(ws.s); got != 0 {
			t.Errorf("%s 单独出现 = %d token，应为 0", ws.name, got)
		}
		// 夹在中文之间：只应数出两个中文，空白不计
		if got := estimateTokens("中" + ws.s + "文"); got != 2 {
			t.Errorf("中%s文 = %d token，应为 2（空白不计）", ws.name, got)
		}
	}
}

func TestEstimateTokensNonWhitespaceStillCounted(t *testing.T) {
	// 对照：真正的可见字符仍按"其他字符"数 1，别把空白类放得太宽
	for _, s := range []string{"·", "—", "、", "…"} {
		if got := estimateTokens(s); got != 1 {
			t.Errorf("estimateTokens(%q) = %d, want 1", s, got)
		}
	}
}
