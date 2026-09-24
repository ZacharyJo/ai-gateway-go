# ai-gateway-go

[English](README.md) | **简体中文**

一个独立的本地 OpenAI 兼容代理服务（独立部署版）。

用于把 AI 客户端（codex / claude / 任意 OpenAI 兼容客户端）的 `/v1/*` 请求转发到第三方 OpenAI 兼容上游，提供：

- **协议适配**：Responses ↔ Messages（Anthropic）/ Chat Completions 自动转换，支持按模型/按上游选择协议（`upstream_wire` / `model_wire`）
- **重试与 429 冷却**：网络错误退避重试、可重试状态码、进程级 429 冷却、reasoning-only 空转重试
- **SSE 流式**：流内软错误检测、`[DONE]` 兜底、空闲超时看护
- **上下文压缩（Headroom）**：大幅减少重复上下文，按 sha256 落盘可回溯
- **监控仪表盘**：`/dashboard` 实时面板，请求成功率/耗时/状态码分布
- **日志轮转 + 错误捕获**：`LOG_DIR/ai-gateway.log` 自动轮转，上游错误响应可落盘脱敏
- **守护子命令**：`proxy start / stop / restart / status / logs` 自管生命周期

## 构建与运行

```bash
make build           # 构建 dist/<goos>-<goarch>/proxy
make proxy-run       # 前台运行
make install         # 安装到 ~/.ai-gateway/bin/proxy
```

## 配置

优先级：**环境变量 > `~/.ai-gateway/config.toml` 的 `[proxy]` 段 > 内置默认**。

最小配置（必须指定上游）：

```bash
export UPSTREAM_BASE="https://api.example.com/v1"
export API_KEY="sk-your-key"   # auth_mode=bearer 时需要
proxy
```

或写配置文件 `~/.ai-gateway/config.toml`（可用 `PROXY_CONFIG` 环境变量覆盖路径）：

```toml
[proxy]
upstream_base = "https://api.example.com/v1"
auth_mode = "bearer"
api_key = "sk-your-key"

# 协议路由：全局走 messages / chat / responses
# upstream_wire = "messages"
# model_wire = "DeepSeek-V4-Flash=chat,gpt-5.6-sol=responses"
```

> 完整配置项见 [examples/config.toml.example](examples/config.toml.example)。

### 认证模式

| `auth_mode` | 行为 |
|-------------|------|
| `none`（默认） | 不注入任何认证头，上游用自己的认证（客户端自带头） |
| `bearer` | 注入 `Authorization: Bearer <api_key>`（客户端已带则不覆盖） |

### 协议路由（`upstream_wire` / `model_wire`）

`/v1/responses` 请求按模型分流到不同上游协议：

| 配置 | 行为 |
|------|------|
| `upstream_wire = "responses"`（默认/空） | GPT 系透传 `/responses`，非 GPT 按模型表走 `/messages` 适配 |
| `upstream_wire = "messages"` | 强制所有模型走 `/messages`（支持 Anthropic Messages 的中转站） |
| `upstream_wire = "chat"` | 强制所有模型走 `/chat/completions`（仅支持 OpenAI Chat 的上游） |
| `model_wire = "模型名=协议"` | 单模型粒度覆盖，优先级最高 |

### 图片桥接（`bridge_imagegen`，默认关）

让**不支持原生图片工具**的模型也能生成/编辑图片。开启后代理向请求注入 `bridge_imagegen` 工具：模型调用它时代理转调上游图片 API（`/images/generations` 或 `/images/edits`），图片落盘到 `<log_dir>/generated-images/`，并把结果写回响应。

```toml
# ~/.ai-gateway/config.toml [proxy] 段
bridge_imagegen_enabled = true            # 启用
image_model = "gpt-image-2.5-sunburst"    # 上游图片模型 ID
image_size = "auto"                       # 尺寸
image_quality = "medium"                  # 质量
image_output_format = "png"               # 落盘格式（png / jpeg / webp）
```

要求与使用：

- **上游需支持 `/v1/images/*`**（OpenAI 兼容图片端点，多数第三方中转支持）。
- 客户端（如 codex）需把 `bridge_imagegen` 当作可用工具；配套的客户端 skill 用 `proxy skill-imagegen <目标目录>` 安装（SKILL.md + `scripts/image_gen.py`，不覆盖已存在的同名文件）。
- 环境变量等价：`BRIDGE_IMAGEGEN_ENABLED` / `IMAGE_MODEL` / `IMAGE_SIZE` / `IMAGE_QUALITY` / `IMAGE_OUTPUT_FORMAT`。

### 客户端接入

**codex**（`~/.codex/config.toml`）：

```toml
model_provider = "ai-gateway"
model = "gpt-5.6-sol"

[model_providers.ai-gateway]
name = "ai-gateway"
base_url = "http://127.0.0.1:8787/v1"
wire_api = "responses"
```

**claude**（设置 `ANTHROPIC_BASE_URL`）：

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787/v1"
```

## 本地路由

| 路径 | 说明 |
|------|------|
| `GET /` 或 `/dashboard` | 监控仪表盘 |
| `GET /api/stats` | 监控 JSON |
| `GET /healthz` | 生效配置 |
| `GET /headroom-lite/<sha256>` | 取回压缩前原文 |
| 其他 | 转发上游 |

## 开机自启

```bash
./scripts/autostart.sh install   # macOS launchd / Linux systemd user 服务
./scripts/autostart.sh status
./scripts/autostart.sh uninstall
```

## 模型 catalog

`model/model-catalog-relay.json` 是给 codex 用的模型列表（改 `~/.codex/config.toml` 的 `model_catalog_json` 指向它），只包含第三方上游实际支持的模型。

## License

[MIT](LICENSE)
