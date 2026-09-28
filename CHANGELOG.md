# Changelog

本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/)；main 合入使用 squash，每个 PR 对应一条记录。

## [v0.3.4] - 2026-09-28

### Fixed

- **responses lite 工具形状**：codex 0.154+ 在 `use_responses_lite` 模式下把工具声明在 `input` 的 `additional_tools` 载体里、按 namespace 分组，而不是顶层 `tools` 字段；适配器此前只读 `tools`，于是**转给上游的请求一个工具都没有**——模型只能凭 developer 提示词里的文字描述去猜格式，把调用吐成原始文本标记（DeepSeek 的 `<｜｜DSML｜｜…>`、`<functions.exec>…</functions.exec>`），而客户端没有从文本反解析工具调用的兜底，表现为"模型只说话、工具从不执行"。现在合并顶层 `tools` 与载体工具，把 namespace 子工具展开为确定性的 `<ns>__<child>`（超 64 字节则前缀截断 + `__` + sha256 前 8 字节），回程还原成 `{name, namespace}`（流式按帧、非流式后处理），并改写 `input` 历史里的 namespace 调用与 namespace 形状的 `tool_choice`。展开名撞车直接报错，不再静默丢工具。
- **图片降级重试不再丢工具**：`rewriteBodyOmitImages` 拿适配前的原始快照重新适配，绕过了工具展开，重试请求一个工具都不带。两条路径现在共用 `applyCodexToolContext`。
- **Chat 路径不再把工具载体当成空消息**：载体项带 `role` 却没有 `content`，Chat 转换器没有兜底分支，会生成一条空 user 消息发给上游（严格网关 400）。Messages 路径本就丢弃未知类型，无此问题。
- **图片降级重试带上游不认的模型名**：`rewriteBodyOmitImages` 重新适配时 `doc["model"]` 仍是客户端原始拼写（如 `claude-opus-5`），而首次适配发的是归一后的名字（如 `Opus 5`）。`adapterContext` 新增 `normalizedModel` 供重新适配使用。
- **别名命中后查表落空**：`SupportsImageInput` / `Classify` 直接拿 `NormalizeModelName` 的返回值查表，而别名值是上游 wire 拼写、未必是折叠形态（`Opus 5` vs `opus 5`），落空即静默 fail-open。新增 `lookupKey` 供查表，`NormalizeModelName` 保持返回 wire 拼写。

### Changed

- **model catalog 钉住三个字段**：`model/model-catalog-relay.json` 逐条目显式写 `tool_mode: "direct"`（客户端 CodeMode 特性默认关，显式声明可防止将来默认变更把适配模型拖进脆弱的 freeform 路径），`shell_type` 统一成规范拼写 `unified_exec`。

### Docs

- README 修正 `use_responses_lite` 的说明：适配器现已支持 lite 形状，该字段从"必需"变成"偏好"；Model catalog 一节记录三个钉住字段为何不能删。

## [v0.3.3] - 2026-09-28

### Fixed

- **原生 Anthropic 透传不再被误判为空流截断**：`tracker.feed` 此前被 `doneNet` 门控，导致 `/messages` 原生透传（`doneNet=false`）每轮 `sawData` 恒为假、被判成空流截断（503）。改为无条件喂 tracker；只有写 `[DONE]` 的收尾仍受 `doneNet` 门控（Anthropic 不认 `[DONE]`）。

### Changed

- **透传路径补协议层截断判定**：干净 EOF 不再等于成功——上游没发终止事件（Responses 的 `response.completed`/`incomplete`/`failed`，或原生 Anthropic 的 `message_stop`）时按截断上报、`request_finish` 记 503，与适配路径同口径。新增 `terminalEventSniffer`（按协议选终止标志串，滑窗检测跨 chunk 切分）。

### Docs

- 澄清 `stream_limits.go` 两处语义：指纹重放冷却只是"不主动诱导"、**并不能真正阻止** codex 重试（未知错误码走 `ApiError::Retryable`，且"流没 `response.completed`"同样触发重试）；`overloadedErrorFrame` 的 `code` codex 并不认识、会忽略，客户端重试的真实原因是这一轮始终没有 `response.completed`。

## [v0.3.2] - 2026-09-28

### Fixed

- **首包预检不再对终态错误重打**：预检遇到首帧即终态错误（如指纹重放冷却）时不再重打——重试必然失败且会延长冷却；改为原样交付，与交付路径的 `isTerminalStreamErrorKind` 口径一致。
- **首包预检事件不再误计为错误**：`stream_prime_failed_frame` → `stream_prime_bad_frame`，避开 monitor `isError` 的 `"failed"` 子串启发式（这两个事件是 warn 级恢复动作，计成 error 会让错误 KPI 虚高）。
- **`tracker.finish` 写入加锁**：与长思考保活 goroutine 并发写 `ResponseWriter` 时经 `wMu` 串行化（此前依赖 `sawDone` 守卫使其为 no-op，不应依赖）。

### Changed

- **裸 `[DONE]` 空轮按截断失败收尾**（Messages 路径与 Chat 路径同口径）：上游只发 `data: [DONE]`、没有任何协议帧时，代理吞掉该 `[DONE]` 并补 `response.failed`（`stream_truncated`）——客户端拿不到 `response.completed` 会自行重试，不再按 200 成功记账。此前 Messages 路径将其原样透传（v0.2.5 的"裸 `[DONE]` 原样透传"行为据此变更）。

## [v0.3.1] - 2026-09-28

### Changed

- **溯源注释中性化**：移植出处注释里的原始项目名改为中性表述（`对应原始 JS 实现 …`），避免与改名后的仓库名混淆。纯注释改动，无功能变化。

## [v0.3.0] - 2026-09-28

### Changed

- **项目更名为 `codex-relay`**（原 `ai-gateway-go`）：
  - Go module 路径 `ai-gateway-go` → `codex-relay`（import 路径同步）。
  - 运行时路径：默认目录 `~/.ai-gateway` → `~/.codex-relay`；日志 `ai-gateway.log` → `codex-relay.log`；守护进程文件 `ai-gateway.{pid,ready,out}` → `codex-relay.{pid,ready,out}`；错误捕获目录 `ai-gateway-captures` → `codex-relay-captures`。
  - Headroom 压缩标记 `[ai-gateway headroom: …]` → `[codex-relay headroom: …]`；监控面板标题同步；Makefile / `scripts/autostart.sh`（launchd label、systemd 服务名）同步。
  - README / examples / issue 模板 / PR 模板同步更新；client 配置示例 provider 名改为 `codex-relay`。

  > **迁移**：把旧的 `~/.ai-gateway/config.toml` 移到 `~/.codex-relay/config.toml`（或用 `PROXY_CONFIG` 指向旧路径），再重启代理。旧目录下的日志/产物可按需迁移。

## [v0.2.6] - 2026-09-28

### Fixed

- **首包预检不再忙旋挂死**：`primeSSEEvent` 对 `(0,nil)` 空读加了次数上限——病理 reader（包装的 idle-timeout / gzip / MultiReader）反复空读时 fail-open 交回正常交付，而不是在写出任何响应字节前死循环。
- **`"error":null` 不再误判为流失败**：Chat 路径（`Push`）与首包预检（`isFailureSSEData`）改用**非 nil** 判定。部分网关每个 chunk 都回显 `"error":null`，此前会被判成失败终态、丢弃整轮并白耗一次重打。
- **截断流的终态帧不再被上游 `[DONE]` 挡在后面**：Messages 路径在有协议事件（`sawEvent`）时吞掉上游 `[DONE]`，由收尾统一补终态——否则客户端在首个 `[DONE]` 停读，补的终态帧永远看不到（v0.2.5 "杜绝断尾流"在该场景失效）。
- **裸 `[DONE]` 空轮不再被记 503**：`ProtocolIncomplete` 尊重 `sawEvent` 门，与 `synthesizeTerminal` 口径一致。
- **失败终态轮不再被记 200 成功**：新增 `TerminalFailed` 信号，Chat/Messages 两条适配路径合成的 `response.failed`（如 `upstream_tool_call_dropped`）现正确反映到 `request_finish` 状态。
- **Messages 路径补长思考保活**：未闭合 `<thinking>` 缓冲期（对下游零输出）现与 Chat 路径一致发 SSE 注释帧，防客户端空闲超时断连。
- **Chat 截断轮的工具丢弃分类修正**：无 `finish_reason` 截断时，被丢弃的工具调用现报 `upstream_tool_call_dropped`（此前恒报 `stream_truncated`，与 Messages 路径不一致）；同时消除 `flushFinish`/`Flush` 里的死分支。
- **空白 name 占位可被真实 name 覆盖**：工具调用首帧 `name` 为纯空白、后续帧给真实 name 时不再永久卡死并误判丢弃。
- **Messages `message_stop` 丢弃工具分支先收尾 message 项**：不再给客户端留 dangling `in_progress` 项。
- **零交付的截断轮分类修正**：`hasSubstantiveOutput` 按收尾清洗后的文本判定，只有标签（如未闭合 `<thinking>`）的块不再被误判为"有产出"。

## [v0.2.5] - 2026-09-27

### Fixed

- **Messages 路径不再留断尾流**：上游 Messages 流在 `message_stop` 之前结束（连接被掐断，或网关 / mock 写完直接 return）时，此前只补 `[DONE]`、从不合成终态帧——客户端拿到的是一条没有终态帧的流，轮次永远挂起。现在与 Chat 路径同口径收尾：有产出按截断报 `response.incomplete`，完全空流报 `stream_truncated`，工具调用被丢弃且无可执行调用时报 `upstream_tool_call_dropped`。
- **截断判定从 TCP 层扩到协议层**：干净 EOF 只说明 HTTP 响应正常终止，不代表这一轮正常收尾。适配路径现在只要没收到上游终止事件（Messages 的 `message_stop` / Chat 的 `finish_reason`），就按截断上报、`request_finish` 记 503，不再把不完整轮次误记成 200 成功。裸 `[DONE]` 流仍按原样透传。

### Changed

- **CI 强制 staticcheck 与竞态检测**：在 `go vet` + `go test` 之外新增 `staticcheck`（版本钉死 v0.8.1）与 `go test -race`，与 PR 模板的要求一致。

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
