---
name: imagegen
description: Generate or edit raster images through the local proxy's OpenAI-compatible image bridge.
---

# Image Generation

## Routing

- Use the `image_gen.py` script (in this skill's `scripts/` dir) to generate or edit
  images. It calls the local proxy's `/v1/images/generations` (or `/v1/images/edits`)
  endpoint, which forwards to the configured image API.
- Do not call Codex's native `image_generation` tool, run ad-hoc shell commands,
  or search the filesystem for an image substitute.
- The proxy forwards the image request to the upstream and the script waits for the
  result; a completed request is a finished turn.

## Generate And Edit

```bash
python3 scripts/image_gen.py generate --prompt "..." --output /path/out.png [--size 1024x1024] [--quality medium] [--format png]
python3 scripts/image_gen.py edit --prompt "..." --image /path/source.png --output /path/out.png
```

For edits, the source image must be an existing absolute path to a PNG/JPEG/WebP.
Use a new versioned filename when saving additional outputs.
Do not resize the result to force a requested size; dimensions are determined by
the image model.

The script prints `[imagegen:pending]` while running and outputs the absolute path
on success. Error output is prefixed `[imagegen:stage]`.

## Execution Contract

1. Wait for the script's terminal result; an in-progress image request is not a
   completed turn.
2. Do not re-submit after a timeout or ambiguous result (`[imagegen:http]` stage).
   The proxy owns retries; the upstream outcome is unknown after a timeout.
3. Never overwrite an existing output file — the script refuses with exit code 2.
4. Display the successful image using an absolute-path Markdown image.
5. Continue project integration when image generation is an intermediate task.

## Overrides

- `IMAGE_API_BASE` env overrides the proxy base URL (default `http://127.0.0.1:8787/v1`).
- `OPENAI_API_KEY` / `CODEX_HOME/auth.json` provide the Authorization token if the
  proxy requires one.
