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

### Image bridge (`bridge_imagegen`, off by default)

Lets models that lack native image generation still produce/edit images. When enabled, the proxy injects a `bridge_imagegen` tool into requests: when the model calls it, the proxy forwards to the upstream image API (`/images/generations` or `/images/edits`), persists the image under `<log_dir>/generated-images/`, and writes the result back into the response.

```toml
# ~/.ai-gateway/config.toml [proxy] section
bridge_imagegen_enabled = true            # enable
image_model = "gpt-image-2.5-sunburst"    # upstream image model id
image_size = "auto"                       # size
image_quality = "medium"                  # quality
image_output_format = "png"               # output format (png / jpeg / webp)
```

Requirements & usage:

- **The upstream must support `/v1/images/*`** (OpenAI-compatible image endpoints; most third-party relays do).
- Both `b64_json` and `url` image responses are supported (relays commonly return `url`; the proxy downloads and persists it).
- Install the companion client skill with `proxy skill-imagegen <target-dir>` (SKILL.md + `scripts/image_gen.py`; existing files are not overwritten).
- Env equivalents: `BRIDGE_IMAGEGEN_ENABLED` / `IMAGE_MODEL` / `IMAGE_SIZE` / `IMAGE_QUALITY` / `IMAGE_OUTPUT_FORMAT`.

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
