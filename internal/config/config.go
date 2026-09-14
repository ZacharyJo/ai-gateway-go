// Package config 提供代理的独立配置加载（不依赖任何 wrapper）。
//
// 第三方独立部署时，配置来源为：环境变量 > PROXY_CONFIG 指定的 TOML 文件 > 内置默认。
// 配置文件默认路径：~/.ai-gateway/config.toml（可用 PROXY_CONFIG 覆盖）。
package config

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ProxyConfig 是 config.toml 的 [proxy] 段：代理模式的运行参数。
// 字段名与代理的环境变量一一对应（小写下划线形式，如 UPSTREAM_BASE → upstream_base）。
// 全部用指针以区分"未配置"与"显式配了零值"（如 port = 0、rate_limit_cooldown_ms = 0）。
// 优先级：环境变量 > 本段 > 内置默认。
type ProxyConfig struct {
	ListenHost          *string `toml:"listen_host"`
	Port                *int    `toml:"port"`
	UpstreamBase        *string `toml:"upstream_base"`
	MaxAttempts         *int    `toml:"max_attempts"`
	RetryDelayMs        *int    `toml:"retry_delay_ms"`
	MaxRetryDelayMs     *int    `toml:"max_retry_delay_ms"`
	RetryStepDelayMs    *int    `toml:"retry_step_delay_ms"`
	RateLimitCooldownMs *int    `toml:"rate_limit_cooldown_ms"`
	RetryableStatuses   *string `toml:"retryable_statuses"`
	StreamIdleTimeoutMs *int    `toml:"stream_idle_timeout_ms"`
	DiagnosticLogging   *bool   `toml:"diagnostic_logging"`
	CaptureErrorBodies  *bool   `toml:"capture_error_bodies"`
	LogDir              *string `toml:"log_dir"`
	ArtifactRetentionH  *int    `toml:"artifact_retention_hours"`

	LogRotationEnabled    *bool  `toml:"log_rotation_enabled"`
	LogRotationMaxBytes   *int64 `toml:"log_rotation_max_bytes"`
	LogRotationKeep       *int   `toml:"log_rotation_keep"`
	LogRotationIntervalMs *int   `toml:"log_rotation_interval_ms"`

	ReasoningOnlyRetryEnabled     *bool   `toml:"reasoning_only_retry_enabled"`
	ReasoningOnlyRetryModels      *string `toml:"reasoning_only_retry_models"`
	ReasoningOnlyRetryMax         *int    `toml:"reasoning_only_retry_max"`
	ReasoningOnlyRetryBufferBytes *int    `toml:"reasoning_only_retry_buffer_bytes"`

	TextOnlyModels        *string `toml:"text_only_models"`
	ResponsesNativeModels *string `toml:"responses_native_models"`
	ModelAliases          *string `toml:"model_aliases"`

	// 认证模式：none（默认，不注入任何认证头）| bearer（注入 api_key）。
	// 独立部署版本默认 none（第三方上游自带认证），不需要显式配置。
	AuthMode *string `toml:"auth_mode"`
	// APIKey 在 auth_mode=bearer 时注入为 Authorization: Bearer <api_key>。
	APIKey *string `toml:"api_key"`

	// UpstreamWire 声明上游支持的协议能力。
	// "responses"（默认/空）：GPT 系透传 /responses，非 GPT 按模型表路由。
	// "messages"：强制所有模型走 Messages 适配。
	// "chat"：强制所有模型走 Chat Completions 适配。
	UpstreamWire *string `toml:"upstream_wire"`
	// ModelWire 按模型粒度覆盖协议，格式 `模型名=协议` 逗号分隔。
	ModelWire *string `toml:"model_wire"`

	HeadroomMode                 *string  `toml:"headroom_lite_mode"`
	HeadroomMinChars             *int     `toml:"headroom_lite_min_chars"`
	HeadroomHeadChars            *int     `toml:"headroom_lite_head_chars"`
	HeadroomTailChars            *int     `toml:"headroom_lite_tail_chars"`
	HeadroomMaxJSONItems         *int     `toml:"headroom_lite_max_json_items"`
	HeadroomStoreDir             *string  `toml:"headroom_lite_store_dir"`
	HeadroomKeepLineContext      *int     `toml:"headroom_lite_keep_line_context"`
	HeadroomMaxSnippets          *int     `toml:"headroom_lite_max_snippets"`
	HeadroomMinSavedChars        *int     `toml:"headroom_lite_min_saved_chars"`
	HeadroomMinSavingsRatio      *float64 `toml:"headroom_lite_min_savings_ratio"`
	HeadroomMinSavedTokens       *int     `toml:"headroom_lite_min_saved_tokens"`
	HeadroomMinTokenSavingsRatio *float64 `toml:"headroom_lite_min_token_savings_ratio"`
	HeadroomLiveZonePolicy       *string  `toml:"headroom_lite_live_zone_policy"`
	HeadroomLiveZoneItems        *int     `toml:"headroom_lite_live_zone_items"`
}

// Load 读取 TOML 配置文件并返回 [proxy] 段（nil 接收者安全）。
// 配置文件路径：PROXY_CONFIG 环境变量指定；未指定时用 ~/.ai-gateway/config.toml。
// 文件不存在或没有 [proxy] 段时返回零值段（所有字段未配置，走默认）。
// 解析失败返回 error：半解析结果会把 *int 置 0，静默用比报错更危险。
func Load() (ProxyConfig, error) {
	path := os.Getenv("PROXY_CONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ProxyConfig{}, nil
		}
		path = filepath.Join(home, ".ai-gateway", "config.toml")
	}
	var wrapper struct {
		Proxy ProxyConfig `toml:"proxy"`
	}
	if _, err := toml.DecodeFile(path, &wrapper); err != nil {
		if os.IsNotExist(err) {
			return ProxyConfig{}, nil // 没有配置文件是正常情况
		}
		return ProxyConfig{}, err
	}
	return wrapper.Proxy, nil
}
