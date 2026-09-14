package proxy

import (
	"regexp"
	"strings"
)

// 模型族识别：
// GPT 系走 Responses 原样透传；非 GPT 走 Responses→Messages（Anthropic 风格）适配。

var gptModelPattern = regexp.MustCompile(`(?i)^gpt(?:-|_|$)`)

// isGptModel 判断是否 GPT 系模型（原样透传 Responses 协议）。
func isGptModel(model string) bool {
	return gptModelPattern.MatchString(strings.TrimSpace(model))
}

// 模型信息有两个来源，各管一段，不要混用：
//   - **模型 catalog**（用户可选的模型列表）决定 model/model-catalog.json 里
//     codex 能选到哪些模型。名单之外的不进 catalog，否则界面上会出现不存在的模型。
//   - **本文件这几张能力表**（内置默认值）决定三种协议（/chat/completions、/messages、
//     /responses）的支持情况、图片输入、上下文窗口。
//
// 两边不重合的部分按下面处理：
//   - catalog 有、能力表未收录（GLM-5.3-Flash、gpt-6-astra）：catalog 保留，
//     能力不猜、走 fail-open（图片照发，让上游给权威错误）。
//   - 能力表有、catalog 没有（Opus 4.8、gemini-3.1-pro）：不进 catalog，
//     但能力条目留着无害 —— 哪天上游放出来就已经是对的。
//
// 能力表里 /chat/completions 与 /messages 是**全模型覆盖**的，所以
// classifyModelForResponses 最后兜底到 Messages 适配不会踩空。

// messageModelCapabilities 记录模型是否支持图片输入（内置默认值）。
// 不在表内的模型按支持图片处理（fail-open）：上游随时新增模型，不进表就静默把图片换成
// 占位文本，比让上游给出权威错误更难排查。
// 键用归一后的名字（trim + 连续空白折为单空格 + 小写 + 别名）；同一模型的 slug 与展示名
// 两种拼法都可能由客户端发来，差异用 modelAliases 归一。
// 注意 model-catalog.json 的 input_modalities 已区分 text/text+image，但代理的
// 图片能力仍以本文件内置表为准，不读 catalog（避免两边口径漂移）。
var messageModelCapabilities = map[string]bool{
	"auto":              false,
	"gemini 3.7 flash":  true,
	"gemini-3.1-pro":    true,
	"glm-5-turbo":       false,
	"glm-5.2":           false,
	"glm-5.3":           false,
	"grok-4.5":          true,
	"grok-4.6":          true,
	"claude haiku 4.5":  true,
	"claude sonnet 4.6": true,
	"claude sonnet 5":   true,
	"opus 4.8":          true,
	"opus 5":            true,
	"kimi k2.6":         true,
	"kimi k3":           true,
	"minimax-m3":        true,
	"deepseek-v4-flash": false,
	"deepseek-v4-pro":   false,
}

// responsesNativeModels 是原生支持 /responses 的模型（GPT 系另由 isGptModel 判定）：
// 这类模型直接透传，不必绕 Messages 适配。
// 键用归一后的正式名；未声明 /responses 支持的模型不在此表内，会走 Messages 适配
// （全模型覆盖，不会踩空）。
//
// 注意：即使上游原生支持 /responses，也只有在客户端不使用 freeform（custom）工具时
// 透传才安全。Codex 的 apply_patch 等 freeform 工具历史记录里带 custom_tool_call
// 类型，这是 Codex 扩展类型、上游不认识；Messages 适配路径会把它还原成标准 tool_use。
// 因此凡是 Codex 实际会用到的模型，不应加入此表。
var responsesNativeModels = map[string]bool{}

// modelAliases 只做**同一模型不同拼法**的归一（slug ↔ display_name），
// 展示名取自 model/model-catalog.json，客户端两种都可能发过来。
// 归一只作用于同一模型：上下文窗口不同的模型变体不算同一模型，不能靠别名归一，
// 否则能力判断与协议路由会张冠李戴。
var modelAliases = map[string]string{
	// relay catalog 的 claude-opus-5 是 Codex 侧 slug；上游要求传 "Opus 5"（精确大小写）。
	"claude-opus-5":     "Opus 5",
	"deepseek v4 pro":   "deepseek-v4-pro",
	"deepseek v4 flash": "deepseek-v4-flash",
	"minimax m3":        "minimax-m3",
	"kimi-k2.6":         "kimi k2.6",
	"sonnet 5":          "claude sonnet 5",
	"sonnet 4.6":        "claude sonnet 4.6",
	"haiku 4.5":         "claude haiku 4.5",
	"grok 4.5":          "grok-4.5",
	"grok 4.6":          "grok-4.6",
}

var multiSpacePattern = regexp.MustCompile(`\s+`)

// foldModelName 折叠模型名：trim、连续空白折为单空格、小写。不做别名归一。
func foldModelName(model string) string {
	return strings.ToLower(multiSpacePattern.ReplaceAllString(strings.TrimSpace(model), " "))
}

// ModelPolicy 是模型能力与协议路由策略：内置表叠加 config.toml/env 的覆盖。
// 新模型上线时改配置即可生效，不必重新编译分发二进制（内置表仍是默认值）。
// 构造后只读，可并发使用。
type ModelPolicy struct {
	capabilities    map[string]bool
	responsesNative map[string]bool
	aliases         map[string]string
	// upstreamWire 声明上游支持的协议能力：
	// "responses"（默认）：GPT 系透传，非 GPT 按 responsesNative 表路由。
	// "messages"：强制所有模型走 Messages 适配，忽略 responsesNative 表。
	// "chat"：强制所有模型走 Chat Completions 适配。
	upstreamWire string
	// modelWire 按模型粒度覆盖协议（优先级高于 upstreamWire），键为归一后模型名。
	// 值：responses | messages。
	modelWire map[string]string
}

// NewModelPolicy 用配置构造策略。cfg 为 nil 时返回纯内置表。
//
// 覆盖语义（与 retryable_statuses 一致，配了就替换而非追加）：
//   - TextOnlyModels 非空 → **整体替换**内置图片能力表：列出的模型 images=false，
//     未列出的走 fail-open。这样既能补新的纯文本模型，也能纠正内置表里标错的条目。
//   - ResponsesNativeModels 非空 → **整体替换**内置的 /responses 原生名单。
//   - ModelAliases 是**合并**：内置别名保留，同名键以配置为准（别名是"同一模型的不同
//     拼法"，没有需要整体清空的场景）。
func NewModelPolicy(cfg *Config) *ModelPolicy {
	p := &ModelPolicy{
		capabilities:    messageModelCapabilities,
		responsesNative: responsesNativeModels,
		aliases:         modelAliases,
	}
	if cfg == nil {
		return p
	}
	if len(cfg.TextOnlyModels) > 0 {
		caps := make(map[string]bool, len(cfg.TextOnlyModels))
		for _, m := range cfg.TextOnlyModels {
			caps[foldModelName(m)] = false
		}
		p.capabilities = caps
	}
	if len(cfg.ResponsesNativeModels) > 0 {
		native := make(map[string]bool, len(cfg.ResponsesNativeModels))
		for _, m := range cfg.ResponsesNativeModels {
			native[foldModelName(m)] = true
		}
		p.responsesNative = native
	}
	if len(cfg.ModelAliases) > 0 {
		aliases := make(map[string]string, len(modelAliases)+len(cfg.ModelAliases))
		for k, v := range modelAliases {
			aliases[k] = v
		}
		for k, v := range cfg.ModelAliases {
			aliases[foldModelName(k)] = foldModelName(v)
		}
		p.aliases = aliases
	}
	p.upstreamWire = cfg.UpstreamWire
	// model_wire 键已在 parseWireList 里做了 ToLower，与 foldModelName 一致（均小写）
	p.modelWire = cfg.ModelWire
	return p
}

// defaultModelPolicy 是纯内置表策略，供不带配置的调用方与测试使用。
var defaultModelPolicy = NewModelPolicy(nil)

// NormalizeModelName 归一模型名：折叠后再走别名表。
func (p *ModelPolicy) NormalizeModelName(model string) string {
	folded := foldModelName(model)
	if alias, ok := p.aliases[folded]; ok {
		return alias
	}
	return folded
}

// SupportsImageInput 返回模型是否支持图片输入。未知模型 fail-open（按支持处理）。
func (p *ModelPolicy) SupportsImageInput(model string) bool {
	supported, known := p.capabilities[p.NormalizeModelName(model)]
	return !known || supported
}

// normalizeModelName 用内置表归一（薄封装，供包内非请求路径使用）。
func normalizeModelName(model string) string { return defaultModelPolicy.NormalizeModelName(model) }

// supportsImageInput 用内置表判定图片能力（薄封装）。
func supportsImageInput(model string) bool { return defaultModelPolicy.SupportsImageInput(model) }

// ModelClass 是 /v1/responses 的模型分类结果（对应 Classify）。
type ModelClass struct {
	Kind           string // gpt_passthrough / responses_passthrough / messages_adapter / chat_adapter
	UpstreamPath   string // messages_adapter: /messages；chat_adapter: /chat/completions
	SupportsImages bool
	Reason         string
}

// IsAdapter 返回是否需要走协议适配（Messages 或 Chat Completions）。
func (m *ModelClass) IsAdapter() bool {
	return m.Kind == "messages_adapter" || m.Kind == "chat_adapter"
}

// IsMessagesAdapter 返回是否走 Messages 适配。
func (m *ModelClass) IsMessagesAdapter() bool { return m.Kind == "messages_adapter" }

// Classify 分类模型：GPT 系与原生支持 /responses 的模型透传，
// 其余走 Messages 适配（能力表里 /messages 是全模型覆盖的，这条兜底不会踩空）。
// 优先级：modelWire 单模型覆盖 > upstreamWire 全局覆盖 > responsesNative 表。
func (p *ModelPolicy) Classify(model string) *ModelClass {
	folded := foldModelName(model)
	// model_wire 单模型覆盖（最高优先级，含 GPT 系）
	if wire := p.modelWire[folded]; wire != "" {
		if wire == "responses" {
			return &ModelClass{Kind: "responses_passthrough", Reason: "model_wire_override"}
		}
		upstreamPath := "/messages"
		kind := "messages_adapter"
		if wire == "chat" {
			upstreamPath = "/chat/completions"
			kind = "chat_adapter"
		}
		return &ModelClass{
			Kind: kind, UpstreamPath: upstreamPath,
			SupportsImages: p.SupportsImageInput(model),
			Reason:         "model_wire_override",
		}
	}
	// GPT 系默认透传
	if isGptModel(model) {
		return &ModelClass{Kind: "gpt_passthrough", Reason: "gpt_model"}
	}
	// 非 GPT：upstream_wire 全局覆盖
	if p.upstreamWire != "messages" && p.responsesNative[p.NormalizeModelName(model)] {
		return &ModelClass{Kind: "responses_passthrough", Reason: "responses_native_model"}
	}
	upstreamPath := "/messages"
	kind := "messages_adapter"
	// upstream_wire="chat" 时走 Chat Completions 适配
	if p.upstreamWire == "chat" {
		upstreamPath = "/chat/completions"
		kind = "chat_adapter"
	}
	return &ModelClass{
		Kind: kind, UpstreamPath: upstreamPath,
		SupportsImages: p.SupportsImageInput(model),
		Reason:         "non_gpt_model",
	}
}

// classifyModelForResponses 用内置表分类（薄封装）。
func classifyModelForResponses(model string) *ModelClass { return defaultModelPolicy.Classify(model) }
