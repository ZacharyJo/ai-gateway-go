package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// hopHeaders 是需要剔除的逐跳头（连接语义，不应转发到上游/客户端）。
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Upstream 负责把 /v1/* 请求转发到上游，带重试与 429 冷却。
type Upstream struct {
	cfg      *Config
	base     *url.URL
	policy   *RetryPolicy
	models   *ModelPolicy // 模型能力/协议路由（内置表 + 配置覆盖）
	cooldown *Cooldown
	client   *http.Client
	log      *Logger
	mon      *Monitor  // 可为 nil（不记录监控事件）
	hr       *Headroom // 可为 nil（HEADROOM_LITE_MODE=off 时不启用）
}

// NewUpstream 构造上游转发器。UpstreamBase 非法时返回错误。
func NewUpstream(cfg *Config, log *Logger, mon *Monitor, hr *Headroom) (*Upstream, error) {
	base, err := url.Parse(cfg.UpstreamBase)
	if err != nil {
		return nil, err
	}
	return &Upstream{
		cfg:  cfg,
		base: base,
		policy: &RetryPolicy{
			MaxAttempts:       cfg.MaxAttempts,
			RetryDelayMs:      cfg.RetryDelayMs,
			MaxRetryDelayMs:   cfg.MaxRetryDelayMs,
			RetryStepDelayMs:  cfg.RetryStepDelayMs,
			RetryableStatuses: cfg.RetryableStatuses,
		},
		models:   NewModelPolicy(cfg),
		cooldown: NewCooldown(cfg.RateLimitCooldownMs),
		client: &http.Client{
			// 显式禁用整体超时：长思考流可能持续数分钟，读 body 不能被掐断。
			// 客户端断开由请求 context 取消，释放上游连接。
			Timeout: 0,
			// 禁止自动跟随 3xx 重定向：POST 被重定向时 Go 默认变成 GET 并丢掉 body，
			// 上游改了地址会静默发出错误的请求。返回最后一次响应让调用方看到真实状态码。
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		log: log,
		mon: mon,
		hr:  hr,
	}, nil
}

// ForwardResult 是一次转发的结果（供调用方记日志/判断签名失效）。
type ForwardResult struct {
	Status   int
	Attempts int
}

// Forward 把请求转发到上游并写回响应，带重试与 429 冷却。返回最终结果（响应已写出）。
// 重试逻辑（指数退避）：
//   - 网络错误：未用尽重试时退避重试，用尽写 503 upstream_unavailable
//   - 可重试状态码（429/5xx、POST /responses 的 404）：退避重试，429 时延长全局冷却
//   - 用尽后交付上游最后一次响应（让客户端看到真实错误）
func (u *Upstream) Forward(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID int64) (*ForwardResult, error) {
	// 请求体先整体读入内存，重试时才能重新发送。
	// 加 64MB 上限防止异常大 body 撑爆内存：正常 API 请求远低于此，超出说明客户端或中间件异常。
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return &ForwardResult{Status: http.StatusBadRequest, Attempts: 1}, nil
	}
	// Headroom 压缩（dry-run 只统计，on 改写 body；off 时 process 原样返回）
	if u.hr != nil && u.hr.Enabled() && r.Method == http.MethodPost {
		body = u.hr.process(r.URL.Path, body, u.log, reqID)
	}
	path := upstreamPath(r.URL.Path)
	// reasoning-only 空转重试资格必须在 prepareAdapter 改写 body/path **之前**判定：
	// 适配器把 /responses 改成 /messages、把 input 改成 messages，
	// planReasoningRetry 判 path==/responses 和读 input 键都会失效，永不命中。
	// 空转重试只对 GPT 系（不走适配器，透传 /responses）有实际意义；
	// 非 GPT 模型即使配入名单也会在 prepareAdapter 之后才检查、自然为 false，
	// 提前判定的代价只是多解析一次 body（无副作用）。
	plan := planReasoningRetry(u.cfg, path, body)
	semanticRetries := 0
	// 协议适配：非 GPT 模型的 POST /v1/responses 转成 Messages 打上游 /messages，响应再转回
	// imageRetried 表示是否已经做过反应式图片降级重试（每次请求最多一次）
	imageRetried := false
	// 图片降级需要原始 Responses 格式 body（适配之前）重新改写；此处先存快照
	originalResponsesBody := body
	adapt := u.prepareAdapter(r, path, &body, reqID)
	if adapt != nil && adapt.err != nil {
		writeProxyError(w, adapt.err.Status, adapt.err.Type, adapt.err.Message)
		return &ForwardResult{Status: adapt.err.Status, Attempts: 1}, nil
	}
	if adapt != nil {
		path = adapt.upstreamPath
	}
	_ = imageRetried // 在内层循环里使用，消除 go vet 未使用警告
	source := sourceKey(r)
	res := &ForwardResult{}
	capture := &captureInfo{
		enabled:      u.cfg.CaptureErrorBodies,
		dir:          u.cfg.LogDir,
		reqID:        reqID,
		method:       r.Method,
		path:         r.URL.Path,
		reqBody:      body,
		upstreamBase: u.cfg.UpstreamBase,
	}

	// 外层循环 = 语义（空转）重试，内层 = HTTP 重试；两者的预算彼此独立。
semanticRetry:
	for {
		for attempt := 1; ; attempt++ {
			res.Attempts = attempt
			u.cooldown.Wait(ctx)

			// 每次尝试独立的可取消 context：空闲超时只掐掉本次上游连接，不影响后续重试。
			// defer 累积至多 MaxAttempts 个（无 goroutine 开销），保证函数返回时全部释放。
			attemptCtx, cancelAttempt := context.WithCancel(ctx)
			defer cancelAttempt()

			resp, err := u.doAttempt(attemptCtx, r, path, body)
			if err != nil {
				if attempt < u.cfg.MaxAttempts {
					delay := u.policy.RetryAfterMs("", attempt, time.Now())
					u.log.Infof("upstream_fetch_retry req=%d %s %s try=%d/%d err=%v retry_in=%dms",
						reqID, r.Method, r.URL.Path, attempt, u.cfg.MaxAttempts, err, delay.Milliseconds())
					u.recordEvent(Event{Name: "upstream_fetch_retry", Level: "info", ReqID: reqID, Method: r.Method, Path: r.URL.Path, Attempts: attempt, MaxAttempt: u.cfg.MaxAttempts, Source: source})
					if !sleepCtx(ctx, delay) {
						return res, ctx.Err()
					}
					continue
				}
				u.log.Errorf("upstream_fetch_failed req=%d %s %s try=%d/%d err=%v",
					reqID, r.Method, r.URL.Path, attempt, u.cfg.MaxAttempts, err)
				u.recordEvent(Event{Name: "upstream_fetch_failed", Level: "error", ReqID: reqID, Method: r.Method, Path: r.URL.Path, Attempts: attempt, MaxAttempt: u.cfg.MaxAttempts, Source: source})
				writeProxyError(w, http.StatusServiceUnavailable, "upstream_unavailable", "exhausted local upstream retries")
				res.Status = http.StatusServiceUnavailable
				return res, nil
			}

			if u.policy.ShouldRetry(resp.StatusCode, r.Method, path) && attempt < u.cfg.MaxAttempts {
				delay := u.policy.RetryAfterMs(resp.Header.Get("Retry-After"), attempt, time.Now())
				if resp.StatusCode == http.StatusTooManyRequests {
					u.cooldown.Extend(delay)
				}
				// 丢弃响应体，释放连接（仅小读一段避免无限读）。
				drain := newIdleTimeoutReader(resp.Body, u.idleTimeout(), cancelAttempt)
				io.Copy(io.Discard, io.LimitReader(drain, 64<<10))
				drain.Close()
				u.log.Infof("upstream_retry req=%d %s %s status=%d try=%d/%d retry_in=%dms",
					reqID, r.Method, r.URL.Path, resp.StatusCode, attempt, u.cfg.MaxAttempts, delay.Milliseconds())
				u.recordEvent(Event{Name: "upstream_retry", Level: "info", ReqID: reqID, Method: r.Method, Path: r.URL.Path, Status: resp.StatusCode, Attempts: attempt, MaxAttempt: u.cfg.MaxAttempts, Source: source})
				if !sleepCtx(ctx, delay) {
					return res, ctx.Err()
				}
				continue
			}

			// 反应式图片降级：上游因图片报错（400/422/501 + 文本特征）时替换图片重试一次。
			// 只对走适配器的请求（adapt != nil）生效，且每次请求最多触发一次（imageRetried 标记）。
			// 这比预防式按能力表猜测更准确——上游给出了权威错误，不会误伤真正支持图片的模型。
			if adapt != nil && !imageRetried &&
				(resp.StatusCode == http.StatusBadRequest || resp.StatusCode == 422 || resp.StatusCode == 501) {
				// 先读完 body 才能判断是否是图片错误
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				resp.Body.Close()
				if isImageUnsupportedError(string(errBody)) {
					imageRetried = true
					// 重新适配原始请求，但强制关闭图片（OmitImages 全部替换为占位文本）
					if rewritten, ok := rewriteBodyOmitImages(originalResponsesBody, adapt); ok {
						body = rewritten
						u.log.Infof("image_fallback req=%d model=%s: 上游拒绝图片，已替换为占位文本并重试",
							reqID, adapt.model)
						u.recordEvent(Event{Name: "image_unsupported_fallback", Level: "info", ReqID: reqID,
							Method: r.Method, Path: r.URL.Path, Status: resp.StatusCode, Source: source})
						continue // continue 会递增 attempt，图片降级占用一次重试配额（MaxAttempts 极小时需注意）
					}
					// 无法重写（body 不含图片或解析失败）：fall-through 正常交付错误响应
					resp = &http.Response{
						StatusCode: resp.StatusCode,
						Header:     resp.Header,
						Body:       io.NopCloser(bytes.NewReader(errBody)),
					}
				} else {
					// 不是图片错误：还原 body 继续正常流程
					resp = &http.Response{
						StatusCode: resp.StatusCode,
						Header:     resp.Header,
						Body:       io.NopCloser(bytes.NewReader(errBody)),
					}
				}
			}

			// 空闲超时看护：上游长时间不吐字节就取消本次连接
			if idle := u.idleTimeout(); idle > 0 {
				resp.Body = newIdleTimeoutReader(resp.Body, idle, func() {
					u.log.Warnf("upstream_stream_idle_timeout req=%d %s %s idle=%s",
						reqID, r.Method, r.URL.Path, idle)
					u.recordEvent(Event{Name: "upstream_stream_idle_timeout", Level: "warn", ReqID: reqID,
						Method: r.Method, Path: r.URL.Path, Source: source})
					cancelAttempt()
				})
			}

			// 交付前：命中空转重试资格的请求先全缓冲判定，空转就丢弃重打（最多一次）
			if plan.eligible && semanticRetries < u.cfg.ReasoningOnlyRetryMax && isSSE(resp) &&
				resp.StatusCode < http.StatusBadRequest {
				buffered, truncated := readAllLimit(resp.Body, u.cfg.ReasoningOnlyRetryBufferBytes)
				resp.Body.Close()
				res.Status = resp.StatusCode
				if truncated {
					// 超过缓冲上限：无法判定，原样交付已读部分（fail-open）
					u.log.Infof("reasoning_only_retry_buffer_exceeded req=%d model=%s bytes>%d",
						reqID, plan.model, u.cfg.ReasoningOnlyRetryBufferBytes)
					u.writeBufferedSSE(w, resp, buffered)
					return res, nil
				}
				// 缓冲为空说明流提前截断（空闲超时/context 取消），非空转，不触发语义重试
				if len(buffered) == 0 {
					u.writeBufferedSSE(w, resp, buffered)
					return res, nil
				}
				if shouldRetryReasoningOnly(summarizeResponsesSSE(string(buffered))) {
					semanticRetries++
					delay := u.policy.RetryAfterMs("", semanticRetries, time.Now())
					u.log.Warnf("reasoning_only_completion req=%d model=%s bytes=%d retry=%d/%d retry_in=%dms",
						reqID, plan.model, len(buffered), semanticRetries, u.cfg.ReasoningOnlyRetryMax, delay.Milliseconds())
					u.recordEvent(Event{Name: "reasoning_only_completion", Level: "warn", ReqID: reqID,
						Method: r.Method, Path: r.URL.Path, Status: resp.StatusCode, Source: source})
					if !sleepCtx(ctx, delay) {
						return res, ctx.Err()
					}
					// 语义重试：用原始 Responses body 重新走适配，而不是已被图片降级改写的 body。
					// 否则用户请求含图片时，降级后的 body 被当作原始请求重打，payload 错乱。
					// prepareAdapter 要求 path=="/responses"，传原始路径 "/responses" 而非已改写的适配路径。
					if adapt != nil {
						body = originalResponsesBody
						responsesPath := "/responses"
						adapt = u.prepareAdapter(r, responsesPath, &body, reqID)
						if adapt != nil && adapt.err != nil {
							writeProxyError(w, adapt.err.Status, adapt.err.Type, adapt.err.Message)
							return &ForwardResult{Status: adapt.err.Status, Attempts: res.Attempts}, nil
						}
						if adapt != nil {
							path = adapt.upstreamPath
						}
					}
					continue semanticRetry // 丢弃这次响应，原样重打一次
				}
				// 不是空转：把缓冲内容原样交付
				u.writeBufferedSSE(w, resp, buffered)
				return res, nil
			}

			// 交付：写响应头 + 流式/复制 body
			res.Status = resp.StatusCode
			// [DONE] 兜底只对 OpenAI 风格端点生效：原生 Anthropic /messages 透传以 message_stop 收尾，
			// 补 [DONE] 会让 Anthropic SDK 拿到无法解析的帧。走适配器时转换器自己会发 [DONE]。
			kind, truncated := u.writeResponse(w, resp, capture, adapt, !isAnthropicNativePath(path))
			if kind != "" {
				// 上游把并发/容量软错误塞进 200 流里；已向下游补 server_overloaded 帧，由客户端重试
				u.log.Warnf("stream_retryable_error req=%d %s %s kind=%s try=%d/%d",
					reqID, r.Method, r.URL.Path, kind, attempt, u.cfg.MaxAttempts)
				u.recordEvent(Event{Name: "stream_retryable_error_detected", Level: "warn", ReqID: reqID,
					Method: r.Method, Path: r.URL.Path, Status: resp.StatusCode, Attempts: attempt,
					MaxAttempt: u.cfg.MaxAttempts, Source: source})
			}
			// 截断/软错误的请求不算成功完成：修正 res.Status 让 request_finish 记真实终态，
			// 避免仪表盘成功率虚高（F2）。
			if truncated || kind != "" {
				res.Status = http.StatusServiceUnavailable
			}
			return res, nil
		}
	}
}

// writeBufferedSSE 把已缓冲的 SSE 响应一次写回客户端（空转判定路径用）。
func (u *Upstream) writeBufferedSSE(w http.ResponseWriter, resp *http.Response, buffered []byte) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(buffered)
	http.NewResponseController(w).Flush()
}

// readAllLimit 最多读 limit 字节；超出返回 truncated=true（已读部分仍返回）。
func readAllLimit(r io.Reader, limit int) (data []byte, truncated bool) {
	data, _ = io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if len(data) > limit {
		return data[:limit], true
	}
	return data, false
}

// recordEvent 记录监控事件（mon 为 nil 时忽略）。
func (u *Upstream) recordEvent(e Event) {
	if u.mon != nil {
		u.mon.Record(e)
	}
}

// idleTimeout 返回流式空闲超时时长（0 表示关闭）。
func (u *Upstream) idleTimeout() time.Duration {
	return time.Duration(u.cfg.StreamIdleTimeoutMs) * time.Millisecond
}

// adapterContext 是一次请求的协议适配上下文（nil 表示不适配，走原样透传）。
type adapterContext struct {
	model         string
	upstreamPath  string
	customTools   map[string]bool // 声明为 freeform（type=custom）的工具名，回程还原 custom_tool_call 用
	isChatAdapter bool            // true=Chat Completions 适配，false=Messages 适配
	err           *AdapterError
}

// prepareAdapter 判断是否需要 Responses→Messages 适配；需要时改写 *body 并返回上下文。
// 只作用于 POST /v1/responses 且模型非 GPT 系；解析失败/非适配场景返回 nil（原样透传）。
func (u *Upstream) prepareAdapter(r *http.Request, path string, body *[]byte, reqID int64) *adapterContext {
	if r.Method != http.MethodPost || path != "/responses" {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(*body, &doc); err != nil {
		return nil // 非 JSON：原样透传
	}
	model := stringifyAny(doc["model"])
	// model 缺失时不适配，原样透传让上游给出权威错误。
	if model == "" {
		return nil
	}
	// 别名归一：把 catalog 展示名（如 "DeepSeek-V4-Flash"）映射到上游实际 model ID。
	// 不管是否走适配器，都要写回 doc——透传路径同样需要让上游收到它认识的 ID。
	normalizedModel := u.models.NormalizeModelName(model)
	if normalizedModel != model {
		doc["model"] = normalizedModel
	}
	class := u.models.Classify(normalizedModel)
	if !class.IsAdapter() {
		// 透传路径：model 已归一，把改写后的 doc 写回 body
		if normalizedModel != model {
			if b, merr := json.Marshal(doc); merr == nil {
				*body = b
			}
		}
		return nil
	}
	customTools := collectCustomToolNames(doc["tools"])
	opts := adapterOptions{SupportsImages: class.SupportsImages}
	var converted map[string]any
	var err error
	if class.IsMessagesAdapter() {
		converted, err = responsesToMessagesRequest(doc, opts)
	} else {
		// chat_adapter：Responses → Chat Completions
		converted, err = responsesToChatRequest(doc, opts)
	}
	if err != nil {
		adapterErr, ok := err.(*AdapterError)
		if !ok {
			adapterErr = newAdapterError("%v", err)
		}
		u.log.Warnf("adapter_request_failed req=%d model=%s err=%v", reqID, model, err)
		return &adapterContext{model: model, upstreamPath: class.UpstreamPath, err: adapterErr}
	}
	b, merr := json.Marshal(converted)
	if merr != nil {
		return &adapterContext{model: model, upstreamPath: class.UpstreamPath, err: newAdapterError("failed to serialize adapted request: %v", merr)}
	}
	*body = b
	u.log.Infof("adapter_applied req=%d model=%s images=%v custom_tools=%d -> %s",
		reqID, model, class.SupportsImages, len(customTools), class.UpstreamPath)
	return &adapterContext{model: model, upstreamPath: class.UpstreamPath, customTools: customTools, isChatAdapter: class.Kind == "chat_adapter"}
}

// doAttempt 发送单个上游请求（不重试）。网络错误以 error 返回。
func (u *Upstream) doAttempt(ctx context.Context, r *http.Request, path string, body []byte) (*http.Response, error) {
	target := *u.base
	// 必须拼接而不是覆盖 base 的路径：UPSTREAM_BASE 通常自带 /v1，
	// 而 upstreamPath 已把客户端的 /v1 前缀剥掉，直接赋值会丢掉 base 的 /v1 导致全部 404。
	target.Path = strings.TrimSuffix(u.base.Path, "/") + path
	target.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 认证注入：独立部署只支持两种模式。
	// none（默认）：完全不注入任何认证头，第三方上游用自己的认证；
	// bearer：只注入 Authorization: Bearer <api_key>。
	switch u.cfg.AuthMode {
	case "bearer":
		copyRequestHeaders(req.Header, r.Header)
		if req.Header.Get("Authorization") == "" && u.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+u.cfg.APIKey)
		}
	default: // "none" 或空
		// 不注入任何认证，客户端的头原样透传（含自定义业务头、Content-Type 等）
		copyRequestHeaders(req.Header, r.Header)
	}
	// body 已整体读入内存、可能被 headroom/适配器改写，ContentLength 必须以实际字节数为准。
	req.ContentLength = int64(len(body))
	return u.client.Do(req)
}

// writeResponse 把上游响应写回客户端：过滤逐跳头，SSE 流式转发，其余解压后复制。
// adapt 非 nil 时把 Messages 响应转回 Responses 格式；doneNet 控制是否给流补 [DONE] 兜底。
// 返回 (软错误类型, 流是否截断)：调用方据此记日志/监控事件，并设置正确的终态状态码。
// 软错误类型未命中时为空串；截断标记为真时表示上游流在 EOF 之前就结束（空闲超时/客户端断开）。
func (u *Upstream) writeResponse(w http.ResponseWriter, resp *http.Response, capture *captureInfo, adapt *adapterContext, doneNet bool) (kind string, truncated bool) {
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(decompressBody(resp), 4<<20))
		capture.write(resp.StatusCode, body)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return "", false
	}
	if isSSE(resp) {
		w.WriteHeader(resp.StatusCode)
		var k string
		var trunc bool
		if adapt != nil && adapt.isChatAdapter {
			nowMs := time.Now().UnixMilli()
			k, trunc = streamSSEChat(w, resp.Body, newChatSSETransformer(adapt.model, nowMs, adapt.customTools))
		} else if adapt != nil {
			k, trunc = streamSSEAdapted(w, resp.Body, newMessagesSSETransformer(adapt.model, adapt.customTools))
		} else {
			k, trunc = streamSSE(w, resp.Body, doneNet)
		}
		return k, trunc
	}
	body, _ := io.ReadAll(io.LimitReader(decompressBody(resp), 8<<20))
	if adapt != nil {
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err == nil {
			now := time.Now()
			if adapt.isChatAdapter {
				if converted, cerr := chatCompletionToResponsesBody(doc, now.UnixMilli(), now.Unix(), adapt.customTools); cerr == nil {
					if b, merr := json.Marshal(converted); merr == nil {
						body = b
					}
				}
			} else {
				if converted, cerr := messagesToResponsesBody(doc, now.UnixMilli(), now.Unix(), adapt.customTools); cerr == nil {
					if b, merr := json.Marshal(converted); merr == nil {
						body = b
					}
				}
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return "", false
}

// upstreamPath 把客户端路径映射到上游路径：剥掉 /v1 前缀（base 自带）。
func upstreamPath(path string) string {
	if path == "/v1" {
		return "/"
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

// isAnthropicNativePath 判断上游路径是否为原生 Anthropic Messages 端点。
// 这类流以 message_stop 收尾、不使用 OpenAI 的 [DONE] 哨兵。
func isAnthropicNativePath(path string) bool {
	return path == "/messages" || strings.HasPrefix(path, "/messages/")
}

// copyRequestHeaders 复制入站请求头到上游：剔除逐跳头/host/content-length，
// 强制 accept-encoding: identity（避免上游压缩干扰 SSE 解析）。
// authorization 原样透传（不改写不注入）。
func copyRequestHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopHeader(k) || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	dst.Set("Accept-Encoding", "identity")
}

// copyResponseHeaders 复制上游响应头到客户端：剔除逐跳头与 content-length/content-encoding
// （SSE 逐块写出无法确定长度；body 已解压）。
func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopHeader(k) || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isHopHeader(k string) bool {
	lower := strings.ToLower(k)
	for _, h := range hopHeaders {
		if lower == strings.ToLower(h) {
			return true
		}
	}
	return false
}

// decompressBody 按 content-encoding 解压非流式响应体（强制 identity 后上游通常不压缩，
// 此处仅防御性处理 gzip/deflate；br 不引入新依赖，原样透传）。
func decompressBody(resp *http.Response) io.Reader {
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip", "x-gzip":
		if zr, err := gzip.NewReader(resp.Body); err == nil {
			// 用 io.NopCloser 包装：调用方只调 resp.Body.Close()，
			// gzip.Reader.Close() 会被跳过；这里让调用方无感知地归还 zr 的内部状态，
			// 同时 gzip checksum 会在 Read 到末尾时自动验证。
			return struct {
				io.Reader
				io.Closer
			}{zr, io.NopCloser(zr)}
		}
	case "deflate":
		return flate.NewReader(resp.Body)
	}
	return resp.Body
}

// writeProxyError 以 OpenAI 风格错误体写响应。
func writeProxyError(w http.ResponseWriter, status int, typ, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message, "type": typ},
	})
}
