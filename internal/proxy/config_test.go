package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// writeProxyToml 写一个只含 [proxy] 段的 config.toml 并让 LoadConfig 读它。
func writeProxyToml(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[proxy]\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_CONFIG", path)
}

func TestLoadConfigFromToml(t *testing.T) {
	clearProxyEnv(t)
	writeProxyToml(t, `
port = 8790
upstream_base = "http://toml-host:9000/v1/"
max_attempts = 3
rate_limit_cooldown_ms = 0
retryable_statuses = "429,503"
diagnostic_logging = true
capture_error_bodies = true
log_dir = "/tmp/toml-log"
artifact_retention_hours = 48
stream_idle_timeout_ms = 0
log_rotation_enabled = false
reasoning_only_retry_enabled = false
reasoning_only_retry_models = "gpt-5.6, glm-5.1"
headroom_lite_mode = "on"
headroom_lite_min_chars = 4096
headroom_lite_min_savings_ratio = 0.5
headroom_lite_live_zone_policy = "all"
`)
	cfg := LoadConfig()

	if cfg.Port != 8790 {
		t.Errorf("Port = %d, want 8790", cfg.Port)
	}
	// 尾部斜杠仍然会被去掉
	if cfg.UpstreamBase != "http://toml-host:9000/v1" {
		t.Errorf("UpstreamBase = %q", cfg.UpstreamBase)
	}
	if cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", cfg.MaxAttempts)
	}
	// 显式配 0 必须生效（指针字段区分"未配置"与"配了 0"）
	if cfg.RateLimitCooldownMs != 0 {
		t.Errorf("RateLimitCooldownMs = %d, want 0", cfg.RateLimitCooldownMs)
	}
	if cfg.StreamIdleTimeoutMs != 0 {
		t.Errorf("StreamIdleTimeoutMs = %d, want 0", cfg.StreamIdleTimeoutMs)
	}
	if len(cfg.RetryableStatuses) != 2 || !cfg.RetryableStatuses[429] || !cfg.RetryableStatuses[503] {
		t.Errorf("RetryableStatuses = %v, want {429,503}", cfg.RetryableStatuses)
	}
	if !cfg.DiagnosticLogging || !cfg.CaptureErrorBodies {
		t.Errorf("bool flags = %v/%v, want true/true", cfg.DiagnosticLogging, cfg.CaptureErrorBodies)
	}
	if cfg.LogDir != "/tmp/toml-log" {
		t.Errorf("LogDir = %q", cfg.LogDir)
	}
	if cfg.ArtifactRetentionH != 48 {
		t.Errorf("ArtifactRetentionH = %d, want 48", cfg.ArtifactRetentionH)
	}
	// 默认开的开关用 toml 显式关掉
	if cfg.LogRotationEnabled {
		t.Error("LogRotationEnabled = true, want false from toml")
	}
	if cfg.ReasoningOnlyRetryEnabled {
		t.Error("ReasoningOnlyRetryEnabled = true, want false from toml")
	}
	if len(cfg.ReasoningOnlyRetryModels) != 2 {
		t.Errorf("Models = %v, want 2 entries", cfg.ReasoningOnlyRetryModels)
	}
	// headroom 段
	if cfg.Headroom.Mode != "on" || !cfg.Headroom.Apply {
		t.Errorf("Headroom.Mode = %q apply=%v, want on/true", cfg.Headroom.Mode, cfg.Headroom.Apply)
	}
	if cfg.Headroom.MinChars != 4096 {
		t.Errorf("Headroom.MinChars = %d, want 4096", cfg.Headroom.MinChars)
	}
	if cfg.Headroom.MinSavingsRatio != 0.5 {
		t.Errorf("Headroom.MinSavingsRatio = %v, want 0.5", cfg.Headroom.MinSavingsRatio)
	}
	if cfg.Headroom.LiveZonePolicy != "all" {
		t.Errorf("Headroom.LiveZonePolicy = %q, want all", cfg.Headroom.LiveZonePolicy)
	}
	// storeDir 未配 → 跟随 toml 里的 log_dir
	if cfg.Headroom.StoreDir != "/tmp/toml-log/headroom-lite-store" {
		t.Errorf("Headroom.StoreDir = %q", cfg.Headroom.StoreDir)
	}
}

func TestEnvOverridesToml(t *testing.T) {
	clearProxyEnv(t)
	writeProxyToml(t, `
port = 8790
upstream_base = "http://toml-host:9000/v1"
headroom_lite_mode = "on"
diagnostic_logging = true
log_rotation_enabled = false
`)
	// 环境变量优先于 config.toml（PORT=... make proxy-run 这类一次性覆盖必须生效）
	t.Setenv(EnvPort, "9999")
	t.Setenv(EnvUpstreamBase, "http://env-host:1234/v1")
	t.Setenv(EnvHeadroomLiteMode, "off")
	t.Setenv(EnvDiagnosticLogging, "0")
	t.Setenv(EnvLogRotationEnabled, "1")
	cfg := LoadConfig()

	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999 (env wins)", cfg.Port)
	}
	if cfg.UpstreamBase != "http://env-host:1234/v1" {
		t.Errorf("UpstreamBase = %q, want env value", cfg.UpstreamBase)
	}
	if cfg.Headroom.Mode != "off" {
		t.Errorf("Headroom.Mode = %q, want off (env wins)", cfg.Headroom.Mode)
	}
	// env "0" 关掉 toml 的 true
	if cfg.DiagnosticLogging {
		t.Error("DiagnosticLogging = true, want false (env 0 wins over toml true)")
	}
	// env "1" 打开 toml 的 false
	if !cfg.LogRotationEnabled {
		t.Error("LogRotationEnabled = false, want true (env 1 wins over toml false)")
	}
}

func TestLoadConfigTomlWithoutProxySection(t *testing.T) {
	clearProxyEnv(t)
	// 只有认证字段、没有 [proxy] 段 → 全部回退内置默认
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("username = \"someone\"\ntoken = \"t\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_CONFIG", path)
	cfg := LoadConfig()
	if cfg.Port != DefaultPort || cfg.UpstreamBase != DefaultUpstreamBase {
		t.Errorf("no [proxy] section should fall back to defaults, got port=%d base=%q", cfg.Port, cfg.UpstreamBase)
	}
	if !cfg.LogRotationEnabled || !cfg.ReasoningOnlyRetryEnabled {
		t.Error("default-on switches should stay on without [proxy] section")
	}
}

// clearProxyEnv 清空所有代理相关环境变量，并把 config.toml 指向不存在的路径，
// 保证测试既不受宿主环境影响、也不读到本机真实的 ~/.ai-gateway/config.toml。
func clearProxyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PROXY_CONFIG", filepath.Join(t.TempDir(), "no-such-config.toml"))
	for _, k := range []string{
		EnvUpstreamBase, EnvListenHost, EnvPort, EnvMaxAttempts,
		EnvRetryDelayMs, EnvMaxRetryDelayMs, EnvRetryStepDelayMs,
		EnvRateLimitCooldownMs, EnvRetryableStatuses, EnvDiagnosticLogging, EnvLogDir,
		EnvStreamIdleTimeoutMs, EnvArtifactRetentionH,
		EnvLogRotationEnabled, EnvLogRotationMaxBytes, EnvLogRotationKeep, EnvLogRotationIntervalMs,
		EnvHeadroomLiteMode, EnvHeadroomLiteMinChars, EnvHeadroomLiteHeadChars, EnvHeadroomLiteTailChars,
		EnvHeadroomLiteMaxJSONItems, EnvHeadroomLiteStoreDir, EnvHeadroomLiteMinSavedChars,
		EnvHeadroomLiteMinSavingsRatio, EnvHeadroomLiteMinSavedTokens, EnvHeadroomLiteMinTokenSavingsR,
		EnvHeadroomLiteLiveZonePolicy, EnvHeadroomLiteLiveZoneItems, EnvHeadroomLiteKeepLineContext, EnvHeadroomLiteMaxSnippets,
		EnvReasoningOnlyRetryEnabled, EnvReasoningOnlyRetryModels, EnvReasoningOnlyRetryMax, EnvReasoningOnlyRetryBufferBytes,
		EnvTextOnlyModels, EnvResponsesNativeModels, EnvModelAliases,
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearProxyEnv(t)
	cfg := LoadConfig()
	if cfg.UpstreamBase != DefaultUpstreamBase {
		t.Errorf("UpstreamBase = %q, want %q", cfg.UpstreamBase, DefaultUpstreamBase)
	}
	if cfg.ListenHost != DefaultListenHost {
		t.Errorf("ListenHost = %q, want %q", cfg.ListenHost, DefaultListenHost)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, DefaultPort)
	}
	if cfg.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", cfg.MaxAttempts, DefaultMaxAttempts)
	}
	if cfg.RetryDelayMs != DefaultRetryDelayMs {
		t.Errorf("RetryDelayMs = %d, want %d", cfg.RetryDelayMs, DefaultRetryDelayMs)
	}
	if cfg.MaxRetryDelayMs != DefaultMaxRetryDelayMs {
		t.Errorf("MaxRetryDelayMs = %d, want %d", cfg.MaxRetryDelayMs, DefaultMaxRetryDelayMs)
	}
	if cfg.RetryStepDelayMs != DefaultRetryStepDelayMs {
		t.Errorf("RetryStepDelayMs = %d, want %d", cfg.RetryStepDelayMs, DefaultRetryStepDelayMs)
	}
	if cfg.RateLimitCooldownMs != DefaultRateLimitCooldownMs {
		t.Errorf("RateLimitCooldownMs = %d, want %d", cfg.RateLimitCooldownMs, DefaultRateLimitCooldownMs)
	}
	if cfg.DiagnosticLogging {
		t.Error("DiagnosticLogging = true, want false")
	}
	if len(cfg.RetryableStatuses) != len(defaultRetryableStatuses) {
		t.Fatalf("RetryableStatuses = %v, want defaults", cfg.RetryableStatuses)
	}
	for _, s := range defaultRetryableStatuses {
		if !cfg.RetryableStatuses[s] {
			t.Errorf("RetryableStatuses missing %d", s)
		}
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv(EnvPort, "8790")
	t.Setenv(EnvUpstreamBase, "http://127.0.0.1:9000/v1/")
	t.Setenv(EnvMaxAttempts, "3")
	t.Setenv(EnvRetryableStatuses, "429, 502, 100, abc")
	t.Setenv(EnvDiagnosticLogging, "1")
	cfg := LoadConfig()
	if cfg.Port != 8790 {
		t.Errorf("Port = %d, want 8790", cfg.Port)
	}
	if cfg.UpstreamBase != "http://127.0.0.1:9000/v1" {
		t.Errorf("UpstreamBase = %q, want trailing slash trimmed", cfg.UpstreamBase)
	}
	if cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", cfg.MaxAttempts)
	}
	if !cfg.DiagnosticLogging {
		t.Error("DiagnosticLogging = false, want true")
	}
	if len(cfg.RetryableStatuses) != 2 {
		t.Errorf("RetryableStatuses = %v, want {429,502}", cfg.RetryableStatuses)
	}
	if !cfg.RetryableStatuses[429] || !cfg.RetryableStatuses[502] {
		t.Errorf("RetryableStatuses missing 429/502: %v", cfg.RetryableStatuses)
	}
	if cfg.RetryableStatuses[100] || cfg.RetryableStatuses[0] {
		t.Errorf("statuses < 400 should be filtered: %v", cfg.RetryableStatuses)
	}
}

func TestLoadConfigMaxRetryDelayNormalized(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv(EnvRetryDelayMs, "5000")
	t.Setenv(EnvMaxRetryDelayMs, "1000")
	cfg := LoadConfig()
	if cfg.MaxRetryDelayMs != 5000 {
		t.Errorf("MaxRetryDelayMs = %d, want >= RetryDelayMs (5000)", cfg.MaxRetryDelayMs)
	}
}

func TestLoadConfigReasoningOnlyRetryDefaults(t *testing.T) {
	clearProxyEnv(t)
	cfg := LoadConfig()
	if !cfg.ReasoningOnlyRetryEnabled {
		t.Error("ReasoningOnlyRetryEnabled = false, want true (默认开)")
	}
	if len(cfg.ReasoningOnlyRetryModels) != 1 || cfg.ReasoningOnlyRetryModels[0] != "gpt-5.6" {
		t.Errorf("Models = %v, want [gpt-5.6]", cfg.ReasoningOnlyRetryModels)
	}
	if cfg.ReasoningOnlyRetryMax != 1 {
		t.Errorf("Max = %d, want 1", cfg.ReasoningOnlyRetryMax)
	}
	if cfg.ReasoningOnlyRetryBufferBytes != DefaultReasoningOnlyRetryBufferBytes {
		t.Errorf("BufferBytes = %d, want %d", cfg.ReasoningOnlyRetryBufferBytes, DefaultReasoningOnlyRetryBufferBytes)
	}
}

func TestLoadConfigReasoningOnlyRetryOverrides(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv(EnvReasoningOnlyRetryEnabled, "0")
	t.Setenv(EnvReasoningOnlyRetryModels, " gpt-5.6 , glm-5.1 ,, ")
	t.Setenv(EnvReasoningOnlyRetryMax, "5")
	cfg := LoadConfig()
	if cfg.ReasoningOnlyRetryEnabled {
		t.Error("ReasoningOnlyRetryEnabled = true, want false (显式 0 关闭)")
	}
	if len(cfg.ReasoningOnlyRetryModels) != 2 ||
		cfg.ReasoningOnlyRetryModels[0] != "gpt-5.6" || cfg.ReasoningOnlyRetryModels[1] != "glm-5.1" {
		t.Errorf("Models = %v, want [gpt-5.6 glm-5.1]（去空白、丢空项）", cfg.ReasoningOnlyRetryModels)
	}
	// 上限被夹到 1：重打一次仍空转就交付，避免首字延迟叠成倍数
	if cfg.ReasoningOnlyRetryMax != 1 {
		t.Errorf("Max = %d, want clamped to 1", cfg.ReasoningOnlyRetryMax)
	}
}

func TestLoadConfigIdleTimeoutAndRetention(t *testing.T) {
	clearProxyEnv(t)
	cfg := LoadConfig()
	if cfg.StreamIdleTimeoutMs != DefaultStreamIdleTimeoutMs {
		t.Errorf("StreamIdleTimeoutMs = %d, want %d", cfg.StreamIdleTimeoutMs, DefaultStreamIdleTimeoutMs)
	}
	if cfg.ArtifactRetentionH != DefaultArtifactRetentionH {
		t.Errorf("ArtifactRetentionH = %d, want %d", cfg.ArtifactRetentionH, DefaultArtifactRetentionH)
	}
	// 0 = 关闭（不是回退默认）
	t.Setenv(EnvStreamIdleTimeoutMs, "0")
	t.Setenv(EnvArtifactRetentionH, "0")
	cfg = LoadConfig()
	if cfg.StreamIdleTimeoutMs != 0 {
		t.Errorf("StreamIdleTimeoutMs = %d, want 0 (explicitly disabled)", cfg.StreamIdleTimeoutMs)
	}
	if cfg.ArtifactRetentionH != 0 {
		t.Errorf("ArtifactRetentionH = %d, want 0 (explicitly disabled)", cfg.ArtifactRetentionH)
	}
	// 负值归零
	t.Setenv(EnvStreamIdleTimeoutMs, "-5")
	if cfg = LoadConfig(); cfg.StreamIdleTimeoutMs != 0 {
		t.Errorf("negative StreamIdleTimeoutMs = %d, want 0", cfg.StreamIdleTimeoutMs)
	}
}

func TestParseRetryableStatuses(t *testing.T) {
	// 未配置 → 默认集
	if m := parseRetryableStatuses(""); len(m) != len(defaultRetryableStatuses) {
		t.Errorf("empty input = %v, want defaults", m)
	}
	// 全部被过滤 → 空集（配置了就按配置，不重试）
	if m := parseRetryableStatuses("abc, 100"); len(m) != 0 {
		t.Errorf("all-filtered input = %v, want empty", m)
	}
}

func TestParseKVList(t *testing.T) {
	got := parseKVList("GLM 6.0=glm-6.0, 别名 = 正式名 ,坏项,=v,k=")
	if len(got) != 2 {
		t.Fatalf("parseKVList = %v, want 2 条（畸形项应丢弃）", got)
	}
	if got["GLM 6.0"] != "glm-6.0" || got["别名"] != "正式名" {
		t.Errorf("parseKVList = %v", got)
	}
	if parseKVList("") != nil || parseKVList("no-equals") != nil {
		t.Error("无有效项时应返回 nil")
	}
}

func TestLoadConfigModelTablesDefaults(t *testing.T) {
	clearProxyEnv(t)
	cfg := LoadConfig()
	if len(cfg.TextOnlyModels) != 0 || len(cfg.ResponsesNativeModels) != 0 || len(cfg.ModelAliases) != 0 {
		t.Errorf("未配置时三张表覆盖应为空，got %v / %v / %v",
			cfg.TextOnlyModels, cfg.ResponsesNativeModels, cfg.ModelAliases)
	}
	// 空覆盖 → 策略等于内置表
	p := NewModelPolicy(cfg)
	if p.SupportsImageInput("GLM-5.2") {
		t.Error("内置表里 GLM-5.2 不支持图片")
	}
	if !p.SupportsImageInput("Kimi K3") {
		t.Error("内置表里 Kimi K3 支持图片")
	}
	if p.Classify("DeepSeek-V4-Flash").IsAdapter() == false {
		t.Error("内置表里 DeepSeek-V4-Flash 应走 Messages 适配（freeform 工具兼容性）")
	}
}

func TestLoadConfigModelTablesFromEnv(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv(EnvTextOnlyModels, "GLM-6.0, brand-new-model")
	t.Setenv(EnvResponsesNativeModels, "brand-new-model")
	t.Setenv(EnvModelAliases, "GLM 6.0=glm-6.0")
	cfg := LoadConfig()
	if len(cfg.TextOnlyModels) != 2 || cfg.TextOnlyModels[0] != "GLM-6.0" {
		t.Errorf("TextOnlyModels = %v", cfg.TextOnlyModels)
	}
	if cfg.ModelAliases["GLM 6.0"] != "glm-6.0" {
		t.Errorf("ModelAliases = %v", cfg.ModelAliases)
	}

	p := NewModelPolicy(cfg)
	// 新模型按配置判为纯文本
	if p.SupportsImageInput("GLM-6.0") || p.SupportsImageInput("brand-new-model") {
		t.Error("配置里列出的模型应判为不支持图片")
	}
	// 别名生效：展示名归一到正式名后同样命中
	if p.SupportsImageInput("GLM 6.0") {
		t.Error("别名归一后应命中 glm-6.0 的能力")
	}
	// 整体替换语义：内置表里的 GLM-5.2 不再被标为纯文本 → fail-open
	if !p.SupportsImageInput("GLM-5.2") {
		t.Error("text_only_models 配了就整体替换内置表，GLM-5.2 应回到 fail-open")
	}
	// /responses 名单同样整体替换
	if p.Classify("brand-new-model").IsAdapter() {
		t.Error("配置里的 responses_native_models 应透传")
	}
	if !p.Classify("DeepSeek-V4-Flash").IsAdapter() {
		t.Error("配置替换后 DeepSeek-V4-Flash 不再在名单里，应走适配")
	}
	// 内置别名仍在（合并语义）
	if !p.SupportsImageInput("Sonnet 5") {
		t.Error("内置别名 Sonnet 5 应保留")
	}
}

func TestNewModelPolicyNilConfig(t *testing.T) {
	p := NewModelPolicy(nil)
	if p.SupportsImageInput("GLM-5.2") {
		t.Error("nil config 应等于纯内置表")
	}
	if p.NormalizeModelName("Sonnet 5") != "claude sonnet 5" {
		t.Errorf("nil config 下别名失效: %q", p.NormalizeModelName("Sonnet 5"))
	}
}
