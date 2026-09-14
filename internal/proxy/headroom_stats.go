package proxy

import (
	"math"
	"sort"
)

// 压缩判定与统计（对应 ai-gateway headroom/stats.mjs）。

// Compression 是一次压缩的产物（对应 JS 里压缩函数返回的对象）。
type Compression struct {
	Kind          string
	Compressed    string
	Lossless      bool
	OriginalChars int
	OriginalText  string
	Hash          string
}

// CandidateStat 是单个候选的分析统计（对应 buildCompressionStat 的返回）。
type CandidateStat struct {
	Path              string
	Key               string
	Role              string
	Type              string
	Kind              string
	Hash              string
	OriginalChars     int
	CompressedChars   int
	SavedChars        int
	SavingsRatio      float64
	OriginalTokens    int
	CompressedTokens  int
	SavedTokens       int
	TokenSavingsRatio float64
	Accepted          bool
	Reason            string
	Error             string
	compressedText    string // 未导出：接受的压缩结果文本
}

// candidate 是待分析候选（对应 collectCandidates 里的 {item, index, path}）。
type candidate struct {
	item  map[string]any
	index int
	path  string
}

// buildCompressionStat 构造候选统计。
func buildCompressionStat(c *candidate, compression *Compression, accepted bool, reason string) *CandidateStat {
	compressedChars := charLen(compression.Compressed)
	savedChars := max(0, compression.OriginalChars-compressedChars)
	savingsRatio := 0.0
	if compression.OriginalChars > 0 {
		savingsRatio = float64(savedChars) / float64(compression.OriginalChars)
	}
	originalTokens := estimateTokens(c.item["output"].(string))
	compressedTokens := estimateTokens(compression.Compressed)
	savedTokens := max(0, originalTokens-compressedTokens)
	tokenRatio := 0.0
	if originalTokens > 0 {
		tokenRatio = float64(savedTokens) / float64(originalTokens)
	}
	return &CandidateStat{
		Path: c.path, Key: "output", Role: roleOf(c.item), Type: typeOf(c.item),
		Kind: compression.Kind, Hash: compression.Hash,
		OriginalChars: compression.OriginalChars, CompressedChars: compressedChars,
		SavedChars: savedChars, SavingsRatio: savingsRatio,
		OriginalTokens: originalTokens, CompressedTokens: compressedTokens, SavedTokens: savedTokens,
		TokenSavingsRatio: tokenRatio, Accepted: accepted, Reason: reason,
	}
}

func roleOf(item map[string]any) string {
	s, _ := item["role"].(string)
	return s
}

func typeOf(item map[string]any) string {
	return stringifyAny(item["type"])
}

// buildFailureStat 构造压缩失败统计。
func buildFailureStat(c *candidate, reason, errMsg string) *CandidateStat {
	output := c.item["output"].(string)
	tokens := estimateTokens(output)
	return &CandidateStat{
		Path: c.path, Key: "output", Role: roleOf(c.item), Type: typeOf(c.item),
		Kind: "failed", Hash: "", OriginalChars: charLen(output), CompressedChars: charLen(output),
		SavedChars: 0, SavingsRatio: 0, OriginalTokens: tokens, CompressedTokens: tokens,
		SavedTokens: 0, TokenSavingsRatio: 0, Accepted: false, Reason: reason, Error: errMsg,
	}
}

// validation 是压缩判定结果。
type validation struct {
	accepted bool
	reason   string
}

// validateCompression 按阈值链判定（对应 validateCompression，顺序即优先级）：
// 1 不变小 → 2 绝对节省字符 → 3 节省比例 → 4 无损（仅按字节）→ 5 token 减少 → 6 token 节省 → 7 token 比例。
func validateCompression(compression *Compression, cfg *HeadroomConfig) validation {
	compressedChars := charLen(compression.Compressed)
	savedChars := max(0, compression.OriginalChars-compressedChars)
	savingsRatio := 0.0
	if compression.OriginalChars > 0 {
		savingsRatio = float64(savedChars) / float64(compression.OriginalChars)
	}
	originalTokens := estimateTokens(compression.OriginalText)
	compressedTokens := estimateTokens(compression.Compressed)
	savedTokens := max(0, originalTokens-compressedTokens)
	tokenRatio := 0.0
	if originalTokens > 0 {
		tokenRatio = float64(savedTokens) / float64(originalTokens)
	}
	if compressedChars >= compression.OriginalChars {
		return validation{accepted: false, reason: "rejected_not_smaller"}
	}
	if savedChars < cfg.MinSavedChars {
		return validation{accepted: false, reason: "below_min_saved_chars"}
	}
	if savingsRatio < cfg.MinSavingsRatio {
		return validation{accepted: false, reason: "below_min_savings_ratio"}
	}
	// 无损改写只删空白，estimateTokens 不数空白，真实 tokenizer 会计数，故仅按字节门槛判定
	if compression.Lossless {
		return validation{accepted: true, reason: "accepted_lossless"}
	}
	if compressedTokens >= originalTokens {
		return validation{accepted: false, reason: "rejected_not_fewer_tokens"}
	}
	if savedTokens < cfg.MinSavedTokens {
		return validation{accepted: false, reason: "below_min_saved_tokens"}
	}
	if tokenRatio < cfg.MinTokenSavingsRatio {
		return validation{accepted: false, reason: "below_min_token_savings_ratio"}
	}
	return validation{accepted: true, reason: "accepted"}
}

// Summary 是压缩汇总（对应 summarizeStats）。
type Summary struct {
	Mode             string         `json:"mode"`
	Eligible         bool           `json:"eligible"`
	Candidates       int            `json:"candidates"`
	Accepted         int            `json:"accepted"`
	Rejected         int            `json:"rejected"`
	OriginalChars    int            `json:"originalChars"`
	CompressedChars  int            `json:"compressedChars"`
	SavedChars       int            `json:"savedChars"`
	SavingsPct       float64        `json:"savingsPct"`
	OriginalTokens   int            `json:"originalTokens"`
	CompressedTokens int            `json:"compressedTokens"`
	SavedTokens      int            `json:"savedTokens"`
	TokenSavingsPct  float64        `json:"tokenSavingsPct"`
	Top              []SummaryEntry `json:"top"`
}

// SummaryEntry 是 top 5 候选。
type SummaryEntry struct {
	Path             string `json:"path"`
	Kind             string `json:"kind"`
	OriginalChars    int    `json:"originalChars"`
	CompressedChars  int    `json:"compressedChars"`
	SavedChars       int    `json:"savedChars"`
	OriginalTokens   int    `json:"originalTokens"`
	CompressedTokens int    `json:"compressedTokens"`
	SavedTokens      int    `json:"savedTokens"`
	Hash             string `json:"hash"`
}

// summarizeStats 汇总分析结果。
func summarizeStats(analysis *Analysis, mode string) *Summary {
	stats := analysis.Accepted
	sum := &Summary{
		Mode: mode, Eligible: analysis.Eligible,
		Candidates: len(analysis.Candidates), Accepted: len(analysis.Accepted), Rejected: len(analysis.Rejected),
	}
	for _, s := range stats {
		sum.OriginalChars += s.OriginalChars
		sum.CompressedChars += s.CompressedChars
		sum.OriginalTokens += s.OriginalTokens
		sum.CompressedTokens += s.CompressedTokens
	}
	sum.SavedChars = max(0, sum.OriginalChars-sum.CompressedChars)
	if sum.OriginalChars > 0 {
		sum.SavingsPct = math.Round(float64(sum.SavedChars)/float64(sum.OriginalChars)*1000) / 10
	}
	sum.SavedTokens = max(0, sum.OriginalTokens-sum.CompressedTokens)
	if sum.OriginalTokens > 0 {
		sum.TokenSavingsPct = math.Round(float64(sum.SavedTokens)/float64(sum.OriginalTokens)*1000) / 10
	}
	// top 5：按 savedTokens 降序、savedChars 降序
	top := append([]*CandidateStat(nil), stats...)
	sort.SliceStable(top, func(i, j int) bool {
		if top[i].SavedTokens != top[j].SavedTokens {
			return top[i].SavedTokens > top[j].SavedTokens
		}
		return top[i].SavedChars > top[j].SavedChars
	})
	if len(top) > 5 {
		top = top[:5]
	}
	for _, s := range top {
		hash := s.Hash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		sum.Top = append(sum.Top, SummaryEntry{
			Path: s.Path, Kind: s.Kind, OriginalChars: s.OriginalChars, CompressedChars: s.CompressedChars,
			SavedChars: s.SavedChars, OriginalTokens: s.OriginalTokens, CompressedTokens: s.CompressedTokens,
			SavedTokens: s.SavedTokens, Hash: hash,
		})
	}
	return sum
}
