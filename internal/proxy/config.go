// Package proxy 实现本地 OpenAI 兼容代理服务（独立部署，不依赖任何 wrapper/内部服务）：
// 监听本地端口，把 /v1/* 请求转发到上游 OpenAI 兼容网关，
// 429/5xx 重试与进程级 429 冷却；另含 Headroom 上下文压缩、监控面板、
// 协议适配器（Responses↔Messages/Chat）等能力。
package proxy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ai-gateway-go/internal/config"
)

// 代理运行时环境变量 KEY（环境变量名即对外接口，部署脚本直接复用）。
const (
	EnvUpstreamBase        = "UPSTREAM_BASE"
	EnvListenHost          = "LISTEN_HOST"
	EnvPort                = "PORT"
	EnvMaxAttempts         = "MAX_ATTEMPTS"
	EnvRetryDelayMs        = "RETRY_DELAY_MS"
	EnvMaxRetryDelayMs     = "MAX_RETRY_DELAY_MS"
	EnvRetryStepDelayMs    = "RETRY_STEP_DELAY_MS"
	EnvRateLimitCooldownMs = "RATE_LIMIT_COOLDOWN_MS"
	EnvRetryableStatuses   = "RETRYABLE_STATUSES"
	EnvStreamIdleTimeoutMs = "STREAM_IDLE_TIMEOUT_MS"
	EnvDiagnosticLogging   = "DIAGNOSTIC_LOGGING"
	EnvCaptureErrorBodies  = "CAPTURE_ERROR_BODIES"
	EnvLogDir              = "LOG_DIR"
	EnvArtifactRetentionH  = "ARTIFACT_RETENTION_HOURS"

	EnvLogRotationEnabled    = "LOG_ROTATION_ENABLED"
	EnvLogRotationMaxBytes   = "LOG_ROTATION_MAX_BYTES"
	EnvLogRotationKeep       = "LOG_ROTATION_KEEP"
	EnvLogRotationIntervalMs = "LOG_ROTATION_INTERVAL_MS"

	EnvHeadroomLiteMode             = "HEADROOM_LITE_MODE"
	EnvHeadroomLiteMinChars         = "HEADROOM_LITE_MIN_CHARS"
	EnvHeadroomLiteHeadChars        = "HEADROOM_LITE_HEAD_CHARS"
	EnvHeadroomLiteTailChars        = "HEADROOM_LITE_TAIL_CHARS"
	EnvHeadroomLiteMaxJSONItems     = "HEADROOM_LITE_MAX_JSON_ITEMS"
	EnvHeadroomLiteStoreDir         = "HEADROOM_LITE_STORE_DIR"
	EnvHeadroomLiteMinSavedChars    = "HEADROOM_LITE_MIN_SAVED_CHARS"
	EnvHeadroomLiteMinSavingsRatio  = "HEADROOM_LITE_MIN_SAVINGS_RATIO"
	EnvHeadroomLiteMinSavedTokens   = "HEADROOM_LITE_MIN_SAVED_TOKENS"
	EnvHeadroomLiteMinTokenSavingsR = "HEADROOM_LITE_MIN_TOKEN_SAVINGS_RATIO"
	EnvHeadroomLiteLiveZonePolicy   = "HEADROOM_LITE_LIVE_ZONE_POLICY"
	EnvHeadroomLiteLiveZoneItems    = "HEADROOM_LITE_LIVE_ZONE_ITEMS"
	EnvHeadroomLiteKeepLineContext  = "HEADROOM_LITE_KEEP_LINE_CONTEXT"
	EnvHeadroomLiteMaxSnippets      = "HEADROOM_LITE_MAX_SNIPPETS"

	EnvReasoningOnlyRetryEnabled     = "REASONING_ONLY_RETRY_ENABLED"
	EnvReasoningOnlyRetryModels      = "REASONING_ONLY_RETRY_MODELS"
	EnvReasoningOnlyRetryMax         = "REASONING_ONLY_RETRY_MAX"
	EnvReasoningOnlyRetryBufferBytes = "REASONING_ONLY_RETRY_BUFFER_BYTES"

	EnvTextOnlyModels        = "TEXT_ONLY_MODELS"
	EnvResponsesNativeModels = "RESPONSES_NATIVE_MODELS"
	EnvModelAliases          = "MODEL_ALIASES"

	// 认证模式：none（默认，不注入任何认证头）| bearer（用 APIKey 注入 Authorization）。
	EnvAuthMode = "AUTH_MODE"
	EnvAPIKey   = "API_KEY"

	// upstream_wire：声明上游支持的协议能力。
	// "responses"（默认/空）：按模型表路由；
	// "messages"：强制全走 Messages 适配；
	// "chat"：强制全走 Chat Completions 适配。
	EnvUpstreamWire = "UPSTREAM_WIRE"
	// model_wire：按模型粒度覆盖协议，格式 `模型名=协议` 逗号分隔。
	EnvModelWire = "MODEL_WIRE"

	// 图片桥接（BRIDGE_IMAGEGEN_ENABLED=1 时生效）：向请求注入 bridge_imagegen 工具，
	// 模型调用时代理转调上游 /images/* API 并把图片落盘写回响应。
	EnvBridgeImagegenEnabled = "BRIDGE_IMAGEGEN_ENABLED"
	EnvBridgeImagegenModel   = "IMAGE_MODEL"
	EnvBridgeImagegenSize    = "IMAGE_SIZE"
	EnvBridgeImagegenQuality = "IMAGE_QUALITY"
	EnvBridgeImagegenFormat  = "IMAGE_OUTPUT_FORMAT"
)

// 默认值（内置）。
const (
	// 独立部署必须显式配置 UPSTREAM_BASE（空串会拒绝启动），避免默认指向某个不该去的上游
	DefaultUpstreamBase        = ""
	DefaultListenHost          = "127.0.0.1"
	DefaultPort                = 8787
	DefaultMaxAttempts         = 5
	DefaultRetryDelayMs        = 2000
	DefaultMaxRetryDelayMs     = 4000
	DefaultRetryStepDelayMs    = 1000
	DefaultRateLimitCooldownMs = 2500
	// 空闲（而非总时长）超时：长思考流会持续吐字节，10 分钟一个字节都没有基本可判定挂死
	DefaultStreamIdleTimeoutMs = 600000
	// 落盘产物保留 7 天：headroom 原文只对进行中的会话有用，过期即无价值
	DefaultArtifactRetentionH = 168

	DefaultLogRotationMaxBytes   = 100 * 1024 * 1024 // 100MB
	DefaultLogRotationKeep       = 5
	DefaultLogRotationIntervalMs = 60000

	DefaultReasoningOnlyRetryModels      = "gpt-5.6"
	DefaultReasoningOnlyRetryMax         = 1
	DefaultReasoningOnlyRetryBufferBytes = 8 * 1024 * 1024 // 8MB

	// 图片桥接默认参数（BRIDGE_IMAGEGEN_ENABLED=1 时生效）。
	DefaultImageModel        = "gpt-image-2.5-sunburst"
	DefaultImageSize         = "auto"
	DefaultImageQuality      = "medium"
	DefaultImageOutputFormat = "png"
)

// 默认可重试状态码（环境变量未配置时使用）。
// 529 = 节点耗尽/服务过载（部分网关使用），与 429 同性质，应重试
var defaultRetryableStatuses = []int{429, 500, 502, 503, 504, 529}

// Config 是代理服务的全部运行时配置，来自环境变量，默认值内置。
type Config struct {
	ListenHost          string
	Port                int
	UpstreamBase        string
	MaxAttempts         int
	RetryDelayMs        int
	MaxRetryDelayMs     int
	RetryStepDelayMs    int
	RateLimitCooldownMs int
	RetryableStatuses   map[int]bool
	StreamIdleTimeoutMs int
	DiagnosticLogging   bool
	CaptureErrorBodies  bool
	LogDir              string
	ArtifactRetentionH  int

	LogRotationEnabled    bool
	LogRotationMaxBytes   int64
	LogRotationKeep       int
	LogRotationIntervalMs int

	// reasoning-only 空转重试（默认开；只对 ReasoningOnlyRetryModels 命中的模型生效）
	ReasoningOnlyRetryEnabled     bool
	ReasoningOnlyRetryModels      []string
	ReasoningOnlyRetryMax         int
	ReasoningOnlyRetryBufferBytes int

	// 模型表覆盖（空表示用内置表）：新模型上线改配置即可，不必重新编译分发。
	TextOnlyModels        []string
	ResponsesNativeModels []string
	ModelAliases          map[string]string

	// 认证模式：none（默认，不注入任何认证头）| bearer（注入 APIKey）。
	// 必须显式配置，不靠 upstream_base 域名猜——猜错会把内部身份静默发给第三方。
	AuthMode string
	// APIKey 在 AuthMode=bearer 时注入为 Authorization: Bearer <APIKey>。
	APIKey string

	// UpstreamWire 声明上游支持的协议能力：
	// ""/"responses"（默认）：GPT 系透传 /responses，非 GPT 按模型表路由（现有行为）。
	// "messages"：强制所有模型走 Messages 适配，忽略 ResponsesNativeModels 表，
	//             适用于只支持 /messages 的中转站（如 api.tu-zi.com）。
	UpstreamWire string
	// ModelWire 按模型粒度覆盖协议，格式 `模型名=协议` 逗号分隔，优先级高于 UpstreamWire。
	// 协议值：responses | messages。
	ModelWire map[string]string

	Headroom HeadroomConfig

	// BridgeImagegenEnabled 为 true 时向请求注入 bridge_imagegen 工具（透传、Messages 与
	// Chat 适配路径都注入，响应侧流式/非流式均会执行），模型调用时代理转调上游图片 API
	// 并把结果写回。默认关（行为变化大，按需打开）。
	BridgeImagegenEnabled bool
	// 图片桥接参数（仅 BridgeImagegenEnabled 时生效）
	ImageModel        string
	ImageSize         string
	ImageQuality      string
	ImageOutputFormat string
}

// headroomConfig 构造 headroom 配置：环境变量 > config.toml [proxy] 段 > 内置默认。
func headroomConfig(logDir string, pc config.ProxyConfig) HeadroomConfig {
	mode := strings.ToLower(envStr(EnvHeadroomLiteMode, orDefault(pc.HeadroomMode, "dry-run")))
	if mode != "off" && mode != "dry-run" && mode != "on" {
		mode = "dry-run"
	}
	storeDir := envStr(EnvHeadroomLiteStoreDir, orDefault(pc.HeadroomStoreDir, ""))
	if storeDir == "" {
		storeDir = logDir + "/headroom-lite-store"
	}
	return HeadroomConfig{
		Mode: mode, Apply: mode == "on",
		MinChars:             max(512, envInt(EnvHeadroomLiteMinChars, orDefault(pc.HeadroomMinChars, 2000))),
		HeadChars:            max(500, envInt(EnvHeadroomLiteHeadChars, orDefault(pc.HeadroomHeadChars, 2500))),
		TailChars:            max(500, envInt(EnvHeadroomLiteTailChars, orDefault(pc.HeadroomTailChars, 2500))),
		MaxJSONItems:         max(5, envInt(EnvHeadroomLiteMaxJSONItems, orDefault(pc.HeadroomMaxJSONItems, 15))),
		StoreDir:             storeDir,
		KeepLineContext:      max(1, envInt(EnvHeadroomLiteKeepLineContext, orDefault(pc.HeadroomKeepLineContext, 3))),
		MaxSnippets:          max(1, envInt(EnvHeadroomLiteMaxSnippets, orDefault(pc.HeadroomMaxSnippets, 20))),
		MinSavedChars:        max(100, envInt(EnvHeadroomLiteMinSavedChars, orDefault(pc.HeadroomMinSavedChars, 1000))),
		MinSavingsRatio:      max(0, envFloat(EnvHeadroomLiteMinSavingsRatio, orDefault(pc.HeadroomMinSavingsRatio, 0.2))),
		MinSavedTokens:       max(0, envInt(EnvHeadroomLiteMinSavedTokens, orDefault(pc.HeadroomMinSavedTokens, 50))),
		MinTokenSavingsRatio: max(0, envFloat(EnvHeadroomLiteMinTokenSavingsR, orDefault(pc.HeadroomMinTokenSavingsRatio, 0.05))),
		LiveZoneItems:        max(0, envInt(EnvHeadroomLiteLiveZoneItems, orDefault(pc.HeadroomLiveZoneItems, 0))),
		LiveZonePolicy:       envStr(EnvHeadroomLiteLiveZonePolicy, orDefault(pc.HeadroomLiveZonePolicy, "latest-per-type")),
	}
}

// LoadConfig 读取 TOML 配置文件与环境变量构造配置（自行加载配置）。
func LoadConfig() *Config {
	pc, _ := config.Load()
	return LoadConfigWith(&pc)
}

// LoadConfigWith 用已加载的 [proxy] 配置段构造代理配置，避免启动时重复读文件。
//
// 优先级：环境变量 > TOML 的 [proxy] 段 > 内置默认。
// 代理的 env 名是文档化的主接口，
// `PORT=8790 make proxy-run` 这种一次性覆盖必须生效；TOML 用来放长期设置。
// 非法值（如 port = "abc"）按 fail-open 回退到下一级。
func LoadConfigWith(pc *config.ProxyConfig) *Config {
	cfg := &Config{
		ListenHost:          envStr(EnvListenHost, orDefault(pc.ListenHost, DefaultListenHost)),
		UpstreamBase:        strings.TrimRight(envStr(EnvUpstreamBase, orDefault(pc.UpstreamBase, DefaultUpstreamBase)), "/"),
		Port:                envInt(EnvPort, orDefault(pc.Port, DefaultPort)),
		MaxAttempts:         max(1, envInt(EnvMaxAttempts, orDefault(pc.MaxAttempts, DefaultMaxAttempts))),
		RetryDelayMs:        max(0, envInt(EnvRetryDelayMs, orDefault(pc.RetryDelayMs, DefaultRetryDelayMs))),
		MaxRetryDelayMs:     max(0, envInt(EnvMaxRetryDelayMs, orDefault(pc.MaxRetryDelayMs, DefaultMaxRetryDelayMs))),
		RetryStepDelayMs:    max(0, envInt(EnvRetryStepDelayMs, orDefault(pc.RetryStepDelayMs, DefaultRetryStepDelayMs))),
		RateLimitCooldownMs: max(0, envInt(EnvRateLimitCooldownMs, orDefault(pc.RateLimitCooldownMs, DefaultRateLimitCooldownMs))),
		RetryableStatuses:   parseRetryableStatuses(envStr(EnvRetryableStatuses, orDefault(pc.RetryableStatuses, ""))),
		StreamIdleTimeoutMs: max(0, envInt(EnvStreamIdleTimeoutMs, orDefault(pc.StreamIdleTimeoutMs, DefaultStreamIdleTimeoutMs))),
		DiagnosticLogging:   envBoolOr(EnvDiagnosticLogging, orDefault(pc.DiagnosticLogging, false)),
		CaptureErrorBodies:  envBoolOr(EnvCaptureErrorBodies, orDefault(pc.CaptureErrorBodies, false)),
		LogDir:              envStr(EnvLogDir, orDefault(pc.LogDir, defaultLogDir())),
		ArtifactRetentionH:  max(0, envInt(EnvArtifactRetentionH, orDefault(pc.ArtifactRetentionH, DefaultArtifactRetentionH))),

		// 默认开，显式 "0"（env）或 false（toml）关闭
		LogRotationEnabled:    envDisableOr(EnvLogRotationEnabled, orDefault(pc.LogRotationEnabled, true)),
		LogRotationMaxBytes:   max(int64(1<<20), envInt64(EnvLogRotationMaxBytes, orDefault(pc.LogRotationMaxBytes, DefaultLogRotationMaxBytes))),
		LogRotationKeep:       max(1, envInt(EnvLogRotationKeep, orDefault(pc.LogRotationKeep, DefaultLogRotationKeep))),
		LogRotationIntervalMs: max(1000, envInt(EnvLogRotationIntervalMs, orDefault(pc.LogRotationIntervalMs, DefaultLogRotationIntervalMs))),

		ReasoningOnlyRetryEnabled: envDisableOr(EnvReasoningOnlyRetryEnabled, orDefault(pc.ReasoningOnlyRetryEnabled, true)),
		ReasoningOnlyRetryModels:  splitCommaList(envStr(EnvReasoningOnlyRetryModels, orDefault(pc.ReasoningOnlyRetryModels, DefaultReasoningOnlyRetryModels))),
		// 上限只能是 0 或 1（按 min(1, max(0, N)) 归一）：重打一次仍空转就交付，
		// 避免把首字延迟叠成整段响应的倍数
		ReasoningOnlyRetryMax:         min(1, max(0, envInt(EnvReasoningOnlyRetryMax, orDefault(pc.ReasoningOnlyRetryMax, DefaultReasoningOnlyRetryMax)))),
		ReasoningOnlyRetryBufferBytes: max(1024, envInt(EnvReasoningOnlyRetryBufferBytes, orDefault(pc.ReasoningOnlyRetryBufferBytes, DefaultReasoningOnlyRetryBufferBytes))),

		TextOnlyModels:        splitCommaList(envStr(EnvTextOnlyModels, orDefault(pc.TextOnlyModels, ""))),
		ResponsesNativeModels: splitCommaList(envStr(EnvResponsesNativeModels, orDefault(pc.ResponsesNativeModels, ""))),
		ModelAliases:          parseKVList(envStr(EnvModelAliases, orDefault(pc.ModelAliases, ""))),

		AuthMode: strings.ToLower(strings.TrimSpace(envStr(EnvAuthMode, orDefault(pc.AuthMode, "none")))),
		APIKey:   envStr(EnvAPIKey, orDefault(pc.APIKey, "")),

		BridgeImagegenEnabled: envBoolOr(EnvBridgeImagegenEnabled, orDefault(pc.BridgeImagegenEnabled, false)),
		ImageModel:            envStr(EnvBridgeImagegenModel, orDefault(pc.ImageModel, DefaultImageModel)),
		ImageSize:             envStr(EnvBridgeImagegenSize, orDefault(pc.ImageSize, DefaultImageSize)),
		ImageQuality:          envStr(EnvBridgeImagegenQuality, orDefault(pc.ImageQuality, DefaultImageQuality)),
		ImageOutputFormat:     envStr(EnvBridgeImagegenFormat, orDefault(pc.ImageOutputFormat, DefaultImageOutputFormat)),
	}
	wire := strings.ToLower(strings.TrimSpace(envStr(EnvUpstreamWire, orDefault(pc.UpstreamWire, ""))))
	if wire != "messages" && wire != "chat" {
		wire = "responses"
	}
	cfg.UpstreamWire = wire
	// model_wire：按模型粒度覆盖协议，格式 `模型名=协议` 逗号分隔
	cfg.ModelWire = parseWireList(envStr(EnvModelWire, orDefault(pc.ModelWire, "")))
	// MAX_RETRY_DELAY_MS 不得小于 RETRY_DELAY_MS（按 max() 归一化）
	if cfg.MaxRetryDelayMs < cfg.RetryDelayMs {
		cfg.MaxRetryDelayMs = cfg.RetryDelayMs
	}
	cfg.Headroom = headroomConfig(cfg.LogDir, *pc)
	return cfg
}

// defaultLogDir 返回代理运行产物的默认目录：~/.ai-gateway。
// 日志、PID、ready、错误捕获、headroom 原文都放这里，避免堆进仓库；
// 绝对路径也让 `proxy start` 从任意 cwd 启动时落到同一处。取不到 home 才回退相对 log/。
func defaultLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "log"
	}
	return filepath.Join(home, ".ai-gateway")
}

// orDefault 返回指针指向的值；指针为 nil（config.toml 里未配置该项）时返回 def。
func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}

// parseRetryableStatuses 解析逗号分隔的状态码列表，过滤掉非整数与 <400 的项。
// 未配置时返回默认集；配置了但全部被过滤则返回空集（不重试）。
func parseRetryableStatuses(s string) map[int]bool {
	m := map[int]bool{}
	if s == "" {
		for _, v := range defaultRetryableStatuses {
			m[v] = true
		}
		return m
	}
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil && n >= 400 {
			m[n] = true
		}
	}
	return m
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

// envBool 按 "1" 判定开启。
func envBool(key string) bool {
	return os.Getenv(key) == "1"
}

// envBoolOr：环境变量未设置时用 def；设置了按 "1" 判定开启。
func envBoolOr(key string, def bool) bool {
	if os.Getenv(key) == "" {
		return def
	}
	return envBool(key)
}

// envDisableOr：环境变量未设置时用 def；设置了按 "0" 判定关闭（其余视为开启）。
// 用于默认开启的开关（日志轮转、空转重试）。
func envDisableOr(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v != "0"
}

// splitCommaList 解析逗号分隔列表，去空白并丢弃空项。
func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseKVList 解析逗号分隔的 `k=v` 列表（如 `GLM 6.0=glm-6.0,别名=正式名`）。
// 缺 `=`、键或值为空的项直接丢弃：宁可少一条别名，也不要把畸形项变成错误映射。
// 全部无效时返回 nil，调用方按"未配置"处理。
func parseKVList(s string) map[string]string {
	var out map[string]string
	for _, p := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(p, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// parseWireList 解析逗号分隔的 `模型名=协议` 列表，只接受 responses / messages 协议值。
// 模型名做 foldModelName 归一（trim + 空白折叠 + 小写），与 ModelPolicy.Classify 的键一致。
func parseWireList(s string) map[string]string {
	var out map[string]string
	for _, p := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(p, "=")
		k = foldModelName(k)
		v = strings.ToLower(strings.TrimSpace(v))
		if !ok || k == "" {
			continue
		}
		if v != "responses" && v != "messages" && v != "chat" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}
