# cursor-deepseek-bridge

A lightweight Go proxy that enables [Cursor](https://cursor.com) to use DeepSeek V4 models (Pro / Flash) via an OpenAI-compatible API.

> **Highlight: Zero-config Thinking toggle.** Just append `-thinking` to any model name (e.g. `deepseek-v4-pro-thinking`) in Cursor, and the proxy automatically enables reasoning, bridges `reasoning_content` across multi-turn conversations, and caches it transparently. No restarts, no config changes — mix thinking and non-thinking models freely in the same session.

> **New: Image OCR Support.** Cursor can't send images directly to DeepSeek (DeepSeek only accepts `text` content). The proxy now runs OCR via RapidOCR on any `image_url` content, extracts the text, and passes it to DeepSeek. The AI can then "see" your screenshots.

## Why this image exists

DeepSeek V4 Pro enables Thinking mode by default and returns `reasoning_content`.  
In multi-turn chat, clients must send that field back on later turns. Cursor currently does not forward `reasoning_content`, which can cause follow-up requests to fail.

Additionally, when Cursor sends images (e.g. screenshots), they arrive as `image_url` content parts. DeepSeek's API only accepts `text` content, so images would cause a deserialization error. This proxy solves both problems.

## Features

- OpenAI-compatible `/v1` endpoint for Cursor
- Fixes DeepSeek V4 Pro multi-turn compatibility issues
- Per-request Thinking mode: use `-thinking` suffixed model names to enable reasoning for specific conversations
- Automatically bridges `reasoning_content` across turns when Thinking mode is active (non-streaming)
- **Image OCR**: automatically extracts text from images using RapidOCR (ONNXRuntime) and passes it to DeepSeek
- Transparently forwards your own DeepSeek API key — no server-side keys needed
- Lightweight multi-stage Docker image (includes Python + RapidOCR)
- Customizable upstream, model mapping, request body size limit
- Simple deployment with Docker / Docker Compose

## Quick Start

```bash
docker run -d \
  --name cursor-deepseek-bridge \
  -p 8080:8080 \
  houzingm/cursor-deepseek-bridge:latest
```

## Cursor Setup

- **OpenAI Base URL**: `https://<your-public-domain>/v1`
- **API Key**: Your DeepSeek API key (obtained from [platform.deepseek.com](https://platform.deepseek.com))
- **Model**: `deepseek-v4-pro` (or your mapped alias)
  - Add `-thinking` suffix to enable reasoning: `deepseek-v4-pro-thinking`
- **Images**: Send screenshots as you normally would — text is extracted via OCR automatically

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `UPSTREAM` | `https://api.deepseek.com` | Upstream endpoint |
| `LISTEN` | `:8080` | Listen address |
| `MAPPED_MODEL` | — | Default mapped model alias target |
| `MODEL_MAP` | — | Extra custom model mappings (`alias=real`, comma-separated) |
| `DS_REASONING_EFFORT` | `high` | Reasoning effort for `-thinking` suffix models (`low` / `medium` / `high`) |
| `DS_CACHE_TTL` | `24h` | Reasoning cache TTL |
| `DS_QUEUE_TTL` | `24h` | Conversation order queue TTL |
| `DS_MAX_REQUEST_BODY` | `32m` | Max HTTP request body size (min: `1m`, max: `256m`). Increase for large images |
| `DS_DEBUG` | `false` | Enable debug logging (set to `true`) |
| `PYTHON` | `python3` | Python executable path (for OCR worker) |

## Image OCR Details

When an `image_url` content part is detected in the request, the proxy:

1. Decodes the base64 image data
2. Sends it to a Python worker subprocess running [RapidOCR](https://github.com/RapidAI/RapidOCR)
3. Receives structured text with bounding boxes and confidence scores
4. Inserts the OCR text into the message content in place of the image

No external OCR executables are needed — `rapidocr` and `onnxruntime` are bundled in the Docker image. If OCR is unavailable or fails, a placeholder `[Image attached - OCR was unable to process this image]` is inserted so that the message is never empty.

## Security Notes

- Do **not** bake secrets into images.
- Pass keys using runtime env vars, Docker secrets, or a secret manager.
- If a key was exposed, revoke and rotate it immediately.

## Notes

Cursor usually cannot call localhost directly as Base URL because of SSRF protections.  
Expose this service through a public HTTPS endpoint (ngrok, Cloudflare Tunnel, or reverse proxy).
