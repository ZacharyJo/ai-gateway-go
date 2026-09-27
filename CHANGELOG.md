# Changelog

本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/)；main 合入使用 squash，每个 PR 对应一条记录。

## [v0.2.4] - 2026-09-27

### Fixed

- **空工具调用不再静默收尾**：上游声明了工具调用但 `name` 缺失或为纯空白（分片只给了 index / id）时，不再静默丢弃后照常收尾——本回合若没有任何可执行工具调用，Chat 与 Messages 两条适配路径都改为下发 `response.failed`（`upstream_tool_call_dropped`）。修复前 codex 会看到"答一句就停、零报错"的 `response.completed`，误判任务已结束。
- **流内错误帧立即转失败终态**：适配路径流中 data 带 `error` 的帧不再被吞掉，直接转 `response.failed + [DONE]`，后续帧全部失效。
- **无 finish_reason 断流不再谎报 completed**：流在 `finish_reason` 之前结束（连接 RST / 网关断流 / 空闲超时）时，已有可执行输出（正文 / reasoning / 合法工具调用）的改用 `response.incomplete`（`incomplete_details.reason=max_output_tokens`）收尾，完全空流转 `response.failed`（`stream_truncated`）；两条分支都保留上游 usage 计量。

### Changed

- **SSE 首包预检与自动重打**：适配路径的 200 SSE 在交付前先读首个完整事件——上游 200 后迟迟不吐数据、流在首个完整事件前就干净结束、或首帧即失败终态时，丢弃该次尝试自动重打一次（上限 1 次）；读到有效首帧即开始透传，已读字节经 `MultiReader` 接回原流，不做整体回退。预检读取上限 256KB，超限 fail-open 交回正常交付，避免阻塞超大首帧。

## [v0.2.3] - 2026-09-27

### Fixed

- **未闭合 think 块不再吞掉答案**：`<thinking>` 未闭合（思考被 max_tokens 截断或模型漏打闭合标签）时，Chat 流式路径不再把整段缓冲转成 reasoning、Messages 路径不再从 `<thinking>` 起整体丢弃——一律去标签后按正文兜底下发，保证每轮都有 assistant message 收尾。修复前 codex 会看到"只有 reasoning 没有 message"的完成轮，当作一轮没执行完提前结束会话。

### Changed

- **截断流兜底（杜绝断尾流）**：上游流在结束哨兵之前以非 EOF 错误收场（空闲超时 / 连接 RST / 网关断流）时，不再给 codex 留一条没有终止帧的断尾流——Chat 适配路径冲刷转换器补发 `response.completed + [DONE]`（未闭合 `<thinking>` 一并按正文兜底），Messages 与透传路径补 `[DONE]` 终止传输。修复前 codex 会把这种流当成"还没执行完"的轮次挂起 / 断开。
- **长思考静默防护**：内嵌 `<thinking>` 缓冲期间（代理对 codex 零输出）以 SSE 注释帧保持流活跃，杜绝客户端因流空闲超时把长思考误判为断流。

## [v0.2.2] - 2026-09-27

### Fixed

- **think 标签变体**：识别口径统一为 `think(?:ing)?`，`<thinking>` 整块与带空白的 `</think >` 不再穿透进正文；流式跨 chunk 切分下，开标签任意位置切开、闭标签恰好切成 `</thinking`（缺 `>`）都会缓冲后再判定，`<` 后跟空白按正文立即下发。

## [v0.2.0] - 2026-09-24

### Added

- **图片桥接 `bridge_imagegen`（默认关）**：向请求注入工具，模型调用时代理转调上游 `/images/*`，图片落盘 `generated-images/`，SSE 与流式路径均支持；新增 `skill-imagegen <dir>` 子命令安装配套客户端 skill。
- **工具调用降级修理**：上游把 `custom` 工具降级成 `function_call` 时回程还原为 `custom_tool_call`（流式 + 非流式）。
- **thinking 整流**：上游报 thinking 签名 / budget 错误时改写请求体并单次重试。
- **reasoning 处理**：inline `<think>` 状态机归入独立 reasoning 项；commentary 与 tool_calls 合并；工具结果媒体块抽取到合成 user 消息。
- **Issue / PR 模板**：`.github/ISSUE_TEMPLATE/` 与 `.github/PULL_REQUEST_TEMPLATE.md`。

### Fixed

- **SSE 流式**：空 200 流补 `[DONE]` 并按截断记账；软错误不再把原始 Chat JSON 透传进 Responses 流；终态错误转 `event:error` 帧；透传路径嗅探 usage。
- **协议适配**：Chat `tool_choice` 支持 auto/required/none 枚举；`finish_reason=length` 按 `completed` 收尾；`created_at` 用 Unix 秒；`tool_use` 首帧 `in_progress`；孤儿 `content_part.done` 守卫；跨帧 `<think>`/minimax 标记清洗。
- **重试与稳定性**：`Retry-After` 钳制到 60s（溢出安全）；429 冷却在最后一次尝试也延展；可重试响应体 drain 加硬超时；reasoning 缓冲超限 / 空流按失败记账；deflate 兼容 zlib；`..` 路径穿越 400 拦截。
- **请求体限制**：超过 64MB 返回 `413 payload_too_large`，不再把截断 body 当完整请求转发。

### Changed

- merge 策略改为 squash-only（main 线性历史）。

## [v0.1.0] - 2026-09-24

- 初始发布：独立部署的本地 OpenAI 兼容代理（协议适配 / 重试与 429 冷却 / SSE 流式 / Headroom 上下文压缩 / 监控面板 / 日志轮转 / 守护进程子命令）。
