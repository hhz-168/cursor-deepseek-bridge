# cursor-deepseek-bridge

A lightweight Go proxy that enables [Cursor](https://cursor.com) to use DeepSeek V4 models (Pro / Flash) via an OpenAI-compatible API.

**Thinking toggle** — append `-thinking` to any model name (e.g. `deepseek-v4-pro-thinking`) in Cursor, and the proxy enables reasoning, bridges `reasoning_content` across turns, and caches it. No restarts needed.

**Image OCR** — automatically extracts text from images via RapidOCR (ONNXRuntime) and passes it to DeepSeek. The AI can then "see" your screenshots.

![1777705403407](image/README.zh-CN/1777705403407.png)

---

## Quick Start

```bash
docker run -d \
  --name cursor-deepseek-bridge \
  -p 8080:8080 \
  houzingm/cursor-deepseek-bridge:latest
```

## Cursor Setup

| Setting                   | Value                                                                         |
| ------------------------- | ----------------------------------------------------------------------------- |
| **OpenAI Base URL** | `https://<your-public-domain>/v1`                                           |
| **API Key**         | Your DeepSeek API key from[platform.deepseek.com](https://platform.deepseek.com) |
| **Model**           | `deepseek-v4-pro` (add `-thinking` suffix to enable reasoning)            |

> Cursor cannot use `localhost` as Base URL due to SSRF protections. Expose this service via ngrok, Cloudflare Tunnel, or a reverse proxy.

## Features

- Fixes DeepSeek V4 multi-turn `reasoning_content` compatibility with Cursor
- Per-request Thinking mode via `-thinking` model suffix (automatically bridges reasoning across turns)
- Image OCR via RapidOCR (pre-installed in Docker image)
- Transparent API key forwarding — no server-side keys needed
- Lightweight multi-stage Docker image (includes Python + RapidOCR)
- Customizable model mapping, upstream endpoint, and request body size

## Environment Variables

| Variable                | Default                      | Description                                            |
| ----------------------- | ---------------------------- | ------------------------------------------------------ |
| `UPSTREAM`            | `https://api.deepseek.com` | Upstream API endpoint                                  |
| `LISTEN`              | `:8080`                    | Listen address                                         |
| `MAPPED_MODEL`        | `deepseek-v4-pro`          | Default model for unknown names                        |
| `MODEL_MAP`           | —                           | Extra model mappings (`alias=real`, comma-separated) |
| `DS_REASONING_EFFORT` | `high`                     | Reasoning effort (`low` / `medium` / `high`)     |
| `DS_CACHE_TTL`        | `24h`                      | Reasoning cache TTL                                    |
| `DS_QUEUE_TTL`        | `24h`                      | Conversation queue TTL                                 |
| `DS_MAX_REQUEST_BODY` | `32m`                      | Max request body size (min `1m`, max `256m`)       |
| `DS_DEBUG`            | `false`                    | Enable debug logging                                   |
| `PYTHON`              | `python3`                  | Python path (for OCR worker)                           |

## How OCR Works

1. Detects `image_url` content in the request
2. Decodes base64 image and sends to Python worker running RapidOCR
3. Inserts recognized text (with bounding boxes and confidence) into message content

If OCR fails, a placeholder `[Image attached - OCR was unable to process this image]` is inserted so the message is never empty.

## Security

Do not bake secrets into images. Pass API keys via runtime env vars, Docker secrets, or a secret manager.
