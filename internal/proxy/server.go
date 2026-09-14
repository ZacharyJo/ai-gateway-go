package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// BinaryName 是代理的可执行文件名（版本/用法输出展示用）。
const BinaryName = "proxy"

// Version 是版本号；发布时可用 ldflags 覆盖：
//
//	go build -ldflags "-X ai-gateway-go/internal/proxy.Version=v1.2.3"
var Version = "0.1.0"

// Author / AuthorEmail 是项目作者信息（--version 展示用）。
const (
	Author      = "ZacharyJo"
	AuthorEmail = "75352196+ZacharyJo@users.noreply.github.com"
)

// redactURL 屏蔽 URL 中的用户名/密码，防止凭据通过 /healthz、/api/stats 泄漏。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
}

// Server 是本地代理 HTTP 服务。
type Server struct {
	cfg        *Config
	log        *Logger
	up         *Upstream
	mon        *Monitor
	hr         *Headroom
	seq        atomic.Int64 // 请求序号（日志/关联用）
	maxRecents int
}

// NewServer 构造代理服务。
func NewServer(cfg *Config, log *Logger) (*Server, error) {
	s := &Server{cfg: cfg, log: log, maxRecents: 300}
	s.mon = NewMonitor(s.maxRecents)
	if cfg.Headroom.Mode != "off" {
		s.hr = newHeadroom(cfg.Headroom)
	}
	up, err := NewUpstream(cfg, log, s.mon, s.hr)
	if err != nil {
		return nil, err
	}
	s.up = up
	return s, nil
}

// ServeHTTP 实现 http.Handler，按路径分流。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/favicon.ico" || r.URL.Path == "/.well-known/appspecific/com.chrome.devtools.json":
		// 浏览器/DevTools 探测请求：直接 204，不转发上游
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/" || r.URL.Path == "/dashboard":
		s.handleIndex(w, r)
	case r.URL.Path == "/api/stats":
		s.handleStats(w, r)
	case r.URL.Path == "/healthz":
		s.handleHealthz(w, r)
	case strings.HasPrefix(r.URL.Path, "/headroom-lite/"):
		s.handleHeadroomOriginal(w, r)
	default:
		s.handleForward(w, r)
	}
}

// handleHeadroomOriginal 返回压缩前原文（/headroom-lite/<sha256>）。
// 只接受 64 位 hex，防路径穿越；on 模式压缩时原文已落盘。
func (s *Server) handleHeadroomOriginal(w http.ResponseWriter, r *http.Request) {
	if s.hr == nil {
		http.NotFound(w, r)
		return
	}
	hash := strings.TrimPrefix(r.URL.Path, "/headroom-lite/")
	text, ok := s.hr.readOriginal(hash)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

// handleForward 转发 /v1/* 到上游并记请求日志与监控事件。
func (s *Server) handleForward(w http.ResponseWriter, r *http.Request) {
	reqID := s.seq.Add(1)
	start := time.Now()
	source := sourceKey(r)
	s.log.Requestf("request_received req=%d %s %s", reqID, r.Method, r.URL.Path)
	s.mon.Record(Event{Name: "request_received", ReqID: reqID, Method: r.Method, Path: r.URL.Path, Source: source})

	res, err := s.up.Forward(r.Context(), w, r, reqID)
	if err != nil {
		// ctx 取消（客户端断开）：不额外写响应，仅记日志
		s.log.Warnf("request_aborted req=%d %s %s err=%v", reqID, r.Method, r.URL.Path, err)
		return
	}
	durMs := time.Since(start).Milliseconds()
	s.log.Requestf("request_finish req=%d %s %s status=%d try=%d/%d dur=%dms",
		reqID, r.Method, r.URL.Path, res.Status, res.Attempts, s.cfg.MaxAttempts, durMs)
	s.mon.Record(Event{Name: "request_finish", ReqID: reqID, Method: r.Method, Path: r.URL.Path, Status: res.Status, Attempts: res.Attempts, MaxAttempt: s.cfg.MaxAttempts, DurMs: durMs, Source: source})
}

// handleStats 返回监控 JSON（/api/stats）。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.mon.Snapshot(redactURL(s.cfg.UpstreamBase)))
}

// handleIndex 返回监控仪表盘页（内嵌 HTML，轮询 /api/stats）。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

// handleHealthz 回显生效配置（/healthz）。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	payload := map[string]any{
		"ok":                  true,
		"upstreamBase":        redactURL(s.cfg.UpstreamBase),
		"listenHost":          s.cfg.ListenHost,
		"port":                s.cfg.Port,
		"maxAttempts":         s.cfg.MaxAttempts,
		"retryDelayMs":        s.cfg.RetryDelayMs,
		"maxRetryDelayMs":     s.cfg.MaxRetryDelayMs,
		"retryStepDelayMs":    s.cfg.RetryStepDelayMs,
		"rateLimitCooldownMs": s.cfg.RateLimitCooldownMs,
		"retryableStatuses":   sortedStatuses(s.cfg.RetryableStatuses),
		"streamIdleTimeoutMs": s.cfg.StreamIdleTimeoutMs,
		"diagnosticLogging":   s.cfg.DiagnosticLogging,
		"captureErrorBodies":  s.cfg.CaptureErrorBodies,
		"logDir":              s.cfg.LogDir,
		"artifactRetentionH":  s.cfg.ArtifactRetentionH,
		"logRotation": map[string]any{
			"enabled":    s.cfg.LogRotationEnabled,
			"maxBytes":   s.cfg.LogRotationMaxBytes,
			"keep":       s.cfg.LogRotationKeep,
			"intervalMs": s.cfg.LogRotationIntervalMs,
		},
		"headroomLite": map[string]any{
			"mode":            s.cfg.Headroom.Mode,
			"minChars":        s.cfg.Headroom.MinChars,
			"minSavedChars":   s.cfg.Headroom.MinSavedChars,
			"minSavingsRatio": s.cfg.Headroom.MinSavingsRatio,
			"minSavedTokens":  s.cfg.Headroom.MinSavedTokens,
			"liveZoneItems":   s.cfg.Headroom.LiveZoneItems,
			"liveZonePolicy":  s.cfg.Headroom.LiveZonePolicy,
			"storeDir":        s.cfg.Headroom.StoreDir,
		},
		"reasoningOnlyRetry": map[string]any{
			"enabled":     s.cfg.ReasoningOnlyRetryEnabled,
			"models":      s.cfg.ReasoningOnlyRetryModels,
			"max":         s.cfg.ReasoningOnlyRetryMax,
			"bufferBytes": s.cfg.ReasoningOnlyRetryBufferBytes,
		},
		"dashboard": "/dashboard",
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// printProxyUsage 打印用法与当前生效配置。配置来自 config.toml [proxy] 段 + 环境变量，
// 无参数前台运行；子命令用于自管生命周期。
func printProxyUsage(w io.Writer, cfg *Config) {
	fmt.Fprintf(w, `用法: %s [子命令]      （运行参数在 config.toml [proxy] 段，或环境变量覆盖）

子命令
    （无）    前台运行（Ctrl-C 停止）
    start     后台守护启动（脱离终端 + log/ai-gateway.pid + 就绪校验；端口被占会报失败）
    stop      停止（SIGTERM 优雅退出，超时再 SIGKILL）
    restart   重启
    status    查看 PID + /healthz
    logs      跟踪日志
    --help / -h   本帮助
    --version / -v 版本

当前生效配置
    监听        %s
    上游        %s
    重试        最多 %d 次，退避 %dms 步进 / 上限 %dms，可重试状态码 %v
    429 冷却    %dms（0=关闭）
    空闲超时    %dms（0=关闭；按空闲而非总时长计时）
    Headroom    %s（minChars=%d）
    空转重试    enabled=%v models=%v max=%d
    日志目录    %s（轮转 enabled=%v，产物保留 %dh）
    错误捕获    %v

本地路由    GET /  或 /dashboard   监控面板
            GET /api/stats         监控 JSON
            GET /healthz           生效配置
            GET /headroom-lite/<sha256>  取回压缩前原文
            其他                   转发上游

配置来源    ~/.ai-gateway/config.toml 的 [proxy] 段；环境变量覆盖之
           （PORT / UPSTREAM_BASE / HEADROOM_LITE_MODE / DIAGNOSTIC_LOGGING 等）
`,
		BinaryName,
		net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port)),
		redactURL(cfg.UpstreamBase),
		cfg.MaxAttempts, cfg.RetryStepDelayMs, cfg.MaxRetryDelayMs, sortedStatuses(cfg.RetryableStatuses),
		cfg.RateLimitCooldownMs,
		cfg.StreamIdleTimeoutMs,
		cfg.Headroom.Mode, cfg.Headroom.MinChars,
		cfg.ReasoningOnlyRetryEnabled, cfg.ReasoningOnlyRetryModels, cfg.ReasoningOnlyRetryMax,
		cfg.LogDir, cfg.LogRotationEnabled, cfg.ArtifactRetentionH,
		cfg.CaptureErrorBodies)
}

// printVersion 打印版本信息。
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "%s v%s (ai-gateway-go)\n", BinaryName, Version)
	fmt.Fprintf(w, "Author: %s <%s>\n", Author, AuthorEmail)
	fmt.Fprintf(w, "License: MIT\n")
	fmt.Fprintf(w, "Go: %s\n", runtime.Version())
}

// sortedStatuses 返回升序状态码列表（healthz 展示用）。
func sortedStatuses(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// loadConfigChecked 加载配置，UPSTREAM_BASE 为空时拒绝启动（独立部署必须显式配置上游）。
func loadConfigChecked() (*Config, error) {
	cfg := LoadConfig()
	if cfg.UpstreamBase == "" {
		return nil, fmt.Errorf("UPSTREAM_BASE 未配置：请在环境变量或 ~/.ai-gateway/config.toml 的 [proxy] 段设置上游地址")
	}
	return cfg, nil
}

// Main 是代理模式入口：加载配置、启动 HTTP 服务、处理信号优雅退出。返回进程退出码。
func Main() int {
	// -h / -v 不依赖上游配置：未配置 UPSTREAM_BASE 也能查看帮助与版本。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			printProxyUsage(os.Stdout, LoadConfig())
			return 0
		case "-v", "--version":
			printVersion(os.Stdout)
			return 0
		}
	}
	// 配置读一次（TOML + 环境变量）。解析失败（如 port = "abc"）直接拒绝启动：
	// 半解析结果会把 *int 置 0，静默用这种配置比报错更危险（端口变成随机端口）。
	cfg, cfgErr := loadConfigChecked()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", cfgErr)
		fmt.Fprintf(os.Stderr, "已忽略配置文件，请修正后重试（或删除该文件回退默认配置）\n")
		return 1
	}
	// 生命周期子命令（让 proxy 自管，无需 make/launchd 包一层）
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			return daemonStart(cfg)
		case "stop":
			return daemonStop(cfg)
		case "restart":
			return daemonRestart(cfg)
		case "status":
			return daemonStatus(cfg)
		case "logs":
			return daemonLogs(cfg)
		default:
			fmt.Fprintf(os.Stderr, "error: 未知参数 %q；支持: start / stop / restart / status / logs / --help / --version\n\n", os.Args[1])
			printProxyUsage(os.Stderr, cfg)
			return 2
		}
	}
	// 后台守护模式下 stderr 被重定向到 .out 文件；日志由 RotatingWriter 统一写 .log，
	// 不再同时写 stderr，避免两个文件内容完全重复。
	logOut := io.Writer(os.Stderr)
	if os.Getenv(envDaemonMarker) == "1" {
		logOut = io.Discard
	}
	log := NewLogger(logOut, cfg.DiagnosticLogging)
	// 日志轮转：LOG_DIR/ai-gateway.log，默认开启
	var rot *RotatingWriter
	if cfg.LogRotationEnabled {
		if w, err := NewRotatingWriter(filepath.Join(cfg.LogDir, "ai-gateway.log"),
			cfg.LogRotationMaxBytes, cfg.LogRotationKeep,
			time.Duration(cfg.LogRotationIntervalMs)*time.Millisecond); err == nil {
			rot = w
			log.SetFile(w)
		}
	}
	if rot != nil {
		defer rot.Close()
	}
	// 落盘产物按 TTL 清理（headroom 原文 / 错误捕获，只写不删会累积）
	retention := NewRetention(cfg.LogDir, time.Duration(cfg.ArtifactRetentionH)*time.Hour, log)
	defer retention.Close()

	srv, err := NewServer(cfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	addr := net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv,
		// 低速连接若一直不发完请求头可以无限占用 goroutine；10s 足够正常客户端。
		// 注意这只限制「读头」阶段，与 StreamIdleTimeoutMs（流式读 body 的看护）正交。
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 信号处理：SIGINT/SIGTERM → 优雅关闭（等待在途请求完成，上限 5s）
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Infof("signal %v received, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	log.Infof("proxy listening on %s, upstream %s", addr, redactURL(cfg.UpstreamBase))
	// 先 bind 再 Serve：bind 成功（端口未被占）后写守护就绪标记，父进程据此判断 start 成功；
	// bind 失败（端口被占/无权限）立刻报错返回，后台 start 的父进程会看到子进程退出。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: 监听 %s 失败: %v\n", addr, err)
		return 1
	}
	markReady(cfg)
	defer clearReady(cfg)
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	log.Infof("proxy stopped")
	return 0
}
