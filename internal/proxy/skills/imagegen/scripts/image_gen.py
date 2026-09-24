#!/usr/bin/env python3
"""Generate and edit images through the local proxy's image bridge.

Contract (mirrors CodexOne imagegen skill):
  - stdout prints `[imagegen:pending]` while running, then the output path on success.
  - stderr prints `[imagegen:<stage>]` prefixed errors; a timeout/ambiguous result
    must NOT be re-submitted automatically (upstream outcome unknown).
  - Never overwrites an existing output file (exit code 2).
"""

import argparse
import base64
import json
import mimetypes
import os
import sys
import tempfile
import urllib.error
import urllib.request
from pathlib import Path

DEFAULT_BASE_URL = os.environ.get("IMAGE_API_BASE", "http://127.0.0.1:8787/v1").rstrip("/")


class ImageError(Exception):
    def __init__(self, stage, message, code):
        super().__init__(message)
        self.stage = stage
        self.code = code


def validate_image(data):
    if not (data.startswith(b"\x89PNG\r\n\x1a\n") or
            data.startswith(b"\xff\xd8\xff") or
            (data.startswith(b"RIFF") and data[8:12] == b"WEBP")):
        raise ImageError("response", "response is not PNG, JPEG, or WebP image data", 12)
    return data


def read_api_key():
    if os.environ.get("OPENAI_API_KEY"):
        return os.environ["OPENAI_API_KEY"]
    home = os.environ.get("CODEX_HOME", str(Path.home() / ".codex"))
    auth_path = Path(home) / "auth.json"
    try:
        auth = json.loads(auth_path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return ""
    for key in ("OPENAI_API_KEY", "api_key", "token"):
        if isinstance(auth.get(key), str):
            return auth[key]
    return ""


def image_data_url(path):
    media_type = mimetypes.guess_type(path.name)[0]
    if media_type not in {"image/png", "image/jpeg", "image/webp"}:
        raise ValueError("edit input must be a PNG, JPEG, or WebP image")
    return f"data:{media_type};base64,{base64.b64encode(path.read_bytes()).decode('ascii')}"


def request_image(args):
    payload = {
        "model": os.environ.get("IMAGE_MODEL", "gpt-image-2.5-sunburst"),
        "prompt": args.prompt,
        "size": args.size,
        "quality": args.quality,
        "output_format": args.format,
        "n": 1,
    }
    endpoint = "images/generations"
    if args.command == "edit":
        payload["image"] = image_data_url(Path(args.image).expanduser().resolve())
        endpoint = "images/edits"
    headers = {"Content-Type": "application/json"}
    api_key = read_api_key()
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    request = urllib.request.Request(
        f"{DEFAULT_BASE_URL}/{endpoint}",
        data=json.dumps(payload).encode("utf-8"),
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=args.timeout) as response:
            body = json.load(response)
    except urllib.error.HTTPError as error:
        raise ImageError("http", f"image API returned HTTP {error.code}; no automatic resubmission", 10) from error
    except urllib.error.URLError as error:
        raise ImageError("http", f"cannot reach image API: {error.reason}", 10) from error
    except (TimeoutError, OSError) as error:
        raise ImageError("http", "request interrupted or timed out; upstream outcome unknown, do not resubmit automatically", 10) from error
    except ValueError as error:
        raise ImageError("response", "image API returned invalid JSON", 11) from error
    try:
        return validate_image(base64.b64decode(body["data"][0]["b64_json"], validate=True))
    except (KeyError, IndexError, TypeError, ValueError) as error:
        raise ImageError("response", "image API response did not contain valid data[0].b64_json", 12) from error


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for name in ("generate", "edit"):
        command = subparsers.add_parser(name)
        command.add_argument("--prompt", required=True)
        command.add_argument("--output", required=True)
        command.add_argument("--size", default="1024x1024")
        command.add_argument("--quality", default="medium")
        command.add_argument("--format", choices=("png", "jpeg", "webp"), default="png")
        command.add_argument("--timeout", type=float, default=300)
        if name == "edit":
            command.add_argument("--image", required=True)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    output = Path(args.output).expanduser().resolve()
    if output.exists():
        print(f"[imagegen:output] refusing to overwrite existing output: {output}", file=sys.stderr)
        return 2
    if not 0 < args.timeout <= 600:
        print("[imagegen:args] timeout must be between 0 and 600 seconds", file=sys.stderr)
        return 2
    temporary = None
    try:
        output.parent.mkdir(parents=True, exist_ok=True)
        print("[imagegen:pending] Request running. Wait for this command's terminal result; do not submit again.", file=sys.stderr, flush=True)
        image = request_image(args)
        # Publish only complete bytes, without replacing an existing output.
        with tempfile.NamedTemporaryFile(dir=output.parent, delete=False) as stream:
            temporary = Path(stream.name)
            stream.write(image)
        os.link(temporary, output)
    except ImageError as error:
        print(f"[imagegen:{error.stage}] {' '.join(str(error).splitlines())}", file=sys.stderr)
        return error.code
    except (OSError, RuntimeError, ValueError) as error:
        print(f"[imagegen:local] {' '.join(str(error).splitlines())}", file=sys.stderr)
        return 13
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    print(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
