package proxy

import "testing"

// 以下两个薄封装仅测试使用：生产路径不需要通过无参全局函数访问内置表策略，
// 直接持有 *ModelPolicy 即可，故从 model_family.go 移到本测试文件。

// supportsImageInput 用内置表判定图片能力（薄封装，仅测试使用）。
func supportsImageInput(model string) bool { return defaultModelPolicy.SupportsImageInput(model) }

// classifyModelForResponses 用内置表分类（薄封装，仅测试使用）。
func classifyModelForResponses(model string) *ModelClass { return defaultModelPolicy.Classify(model) }

// TestAliasTargetNotFoldedStillHitsCapabilityTable 锁定：别名表的值是**上游 wire 拼写**
// （内置表里 "claude-opus-5" → "Opus 5"，带大写），而能力表键统一是折叠形态（"opus 5"）。
// 查表若直接拿 NormalizeModelName 的返回值，别名命中后会落空 → fail-open 把纯文本模型
// 误判成支持图片。回归：lookupKey 折叠一次再查表。
func TestAliasTargetNotFoldedStillHitsCapabilityTable(t *testing.T) {
	cfg := &Config{TextOnlyModels: []string{"opus 5"}} // 把 opus 5 标成纯文本
	p := NewModelPolicy(cfg)

	// wire 拼写必须原样保留（大小写与空格都是上游要求的一部分）
	if got := p.NormalizeModelName("claude-opus-5"); got != "Opus 5" {
		t.Fatalf("NormalizeModelName = %q, want %q（别名值是 wire 拼写，不可折叠）", got, "Opus 5")
	}
	if p.SupportsImageInput("claude-opus-5") {
		t.Error("别名命中后查表落空 → 纯文本模型被 fail-open 误判为支持图片")
	}
	// 别名源（客户端 slug）与别名目标（上游 wire 名）两种拼法都要判成纯文本
	if p.SupportsImageInput("Opus 5") {
		t.Error("别名目标拼法也应命中能力表")
	}
}

// TestAliasTargetNotFoldedStillHitsResponsesNativeTable 同上，锁定路由表一侧：
// responsesNative 的键也是折叠形态，别名命中后查表落空会把 /responses 原生模型
// 误判成需要 Messages 适配。
func TestAliasTargetNotFoldedStillHitsResponsesNativeTable(t *testing.T) {
	cfg := &Config{ResponsesNativeModels: []string{"opus 5"}}
	p := NewModelPolicy(cfg)
	if class := p.Classify("claude-opus-5"); class.Kind != "responses_passthrough" {
		t.Errorf("Classify(claude-opus-5).Kind = %q, want responses_passthrough（路由表查找键未折叠会漏）", class.Kind)
	}
}
