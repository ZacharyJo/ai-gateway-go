package proxy

import "regexp"

// tokenPattern 对应 ai-gateway headroom/token_estimator.mjs 的正则，必须一比一复刻：
// 单个中文字符 / 连续字母 / 连续数字 / 单个其他非空白字符。
//
// 空白类必须显式列全 JS `\s` 的成员：Go 的 `\s` 只有 ASCII 的 [\t\n\f\r ]，
// 而 JS 还含 \v、U+00A0、U+1680、U+2000-200A、U+2028、U+2029、U+202F、U+205F、
// U+3000（全角空格）、U+FEFF。少列一个，含全角空格的中文就会被当成"其他字符"多算
// 一个 token，headroom 的 MinSavedTokens / MinTokenSavingsRatio 阈值相对参考实现漂移。
const jsWhitespaceClass = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var tokenPattern = regexp.MustCompile(`[一-鿿]|[A-Za-z]+|\d+|[^` + jsWhitespaceClass + `A-Za-z\d一-鿿]`)

var allLettersPattern = regexp.MustCompile(`^[A-Za-z]+$`)
var allDigitsPattern = regexp.MustCompile(`^\d+$`)

// estimateTokens 估算文本 token 数（Headroom 阈值链的分母，实现偏差会导致接受/拒绝漂移）：
// 连续字母 → max(1, ceil(len/4))；连续数字 → max(1, ceil(len/3))；其他（单个中文/标点）→ 1。
// 空白不计入（正则不匹配空白）。
func estimateTokens(text string) int {
	total := 0
	for _, part := range tokenPattern.FindAllString(text, -1) {
		switch {
		case allLettersPattern.MatchString(part):
			total += max(1, (len(part)+3)/4)
		case allDigitsPattern.MatchString(part):
			total += max(1, (len(part)+2)/3)
		default:
			total++
		}
	}
	return total
}
