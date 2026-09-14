# ai-gateway-go

**English** | [简体中文](README.zh-CN.md)

A standalone local OpenAI-compatible proxy service (independent deployment).

It forwards `/v1/*` requests from AI clients (codex / claude / any OpenAI-compatible client) to third-party OpenAI-compatible upstreams, providing:

- **Protocol adaptation**: automatic conversion between Responses ↔ Messages (Anthropic) / Chat Completions, with per-model / per-upstream protocol selection (`upstream_wire` / `model_wire`)
- **Retry & 429 cooldown**: exponential-backoff retry on network errors, configurable retryable status codes, process-wide 429 cooldown, reasoning-only idle retry
- **SSE streaming**: in-stream soft-error detection, `[DONE]` fallback, idle-timeout watchdog
- **Context compression (Headroom)**: significantly reduces repeated context, persisted by sha256 for traceability
- **Monitoring dashboard**: `/dashboard` real-time panel with request success rate / latency / status-code distribution
- **Log rotation + error capture**: `LOG_DIR/ai-gateway.log` auto-rotates; upstream error bodies can be captured (redacted) to disk
- **Daemon subcommands**: `proxy start / stop / restart / status / logs` self-managed lifecycle

## Build & Run

```bash
make build           # build dist/<goos>-<goarch>/proxy
make proxy-run       # run in foreground
make install         # install to ~/.ai-gateway/bin/proxy
```

## Configuration

Priority: **environment variables > `[proxy]` section of `~/.ai-gateway/config.toml` > built-in defaults**.

Minimal configuration (an upstream is required):

```bash
export UPSTREAM_BASE="https://api.example.com/v1"
export API_KEY="sk-your-key"   # required when auth_mode=bearer
proxy
```

Or write a config file `~/.ai-gateway/config.toml` (override the path with the `PROXY_CONFIG` environment variable):

```toml
[proxy]
upstream_base = "https://api.example.com/v1"
auth_mode = "bearer"
api_key = "sk-your-key"

# Protocol routing: global messages / chat / responses
# upstream_wire = "messages"
# model_wire = "DeepSeek-V4-Flash=chat,gpt-5.6-sol=responses"
```

> See [examples/config.toml.example](examples/config.toml.example) for the full option list.

### Auth modes

| `auth_mode` | Behavior |
|-------------|----------|
| `none` (default) | No auth header injected; upstream uses its own auth (client sends its own headers) |
| `bearer` | Injects `Authorization: Bearer <api_key>` (does not override an existing client header) |

### Protocol routing (`upstream_wire` / `model_wire`)

`/v1/responses` requests are routed by model to different upstream protocols:

| Config | Behavior |
|--------|----------|
| `upstream_wire = "responses"` (default/empty) | GPT-family passthrough `/responses`; non-GPT routed to `/messages` per the model table |
| `upstream_wire = "messages"` | Force all models to `/messages` (for relays supporting only Anthropic Messages) |
| `upstream_wire = "chat"` | Force all models to `/chat/completions` (for upstreams supporting only OpenAI Chat) |
| `model_wire = "model=protocol"` | Per-model override, highest priority |

### Client setup

**codex** (`~/.codex/config.toml`):

```toml
model_provider = "ai-gateway"
model = "gpt-5.6-sol"

[model_providers.ai-gateway]
name = "ai-gateway"
base_url = "http://127.0.0.1:8787/v1"
wire_api = "responses"
```

**claude** (set `ANTHROPIC_BASE_URL`):

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787/v1"
```

## Local routes

| Path | Description |
|------|-------------|
| `GET /` or `/dashboard` | Monitoring dashboard |
| `GET /api/stats` | Monitoring JSON |
| `GET /healthz` | Effective configuration |
| `GET /headroom-lite/<sha256>` | Retrieve pre-compression original text |
| others | Forward to upstream |

## Autostart

```bash
./scripts/autostart.sh install   # macOS launchd / Linux systemd user service
./scripts/autostart.sh status
./scripts/autostart.sh uninstall
```

## Model catalog

`model/model-catalog-relay.json` is the model list for codex (point `~/.codex/config.toml`'s `model_catalog_json` at it); it only contains models actually supported by third-party upstreams.

## License

[MIT](LICENSE)
