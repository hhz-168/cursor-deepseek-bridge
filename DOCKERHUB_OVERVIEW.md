# cursor-deepseek-bridge

A lightweight Go proxy that enables [Cursor](https://cursor.com) to use DeepSeek V4 models (Pro / Flash) via an OpenAI-compatible API.

> **Highlight: Zero-config Thinking toggle.** Just append `-thinking` to any model name (e.g. `deepseek-v4-pro-thinking`) in Cursor, and the proxy automatically enables reasoning, bridges `reasoning_content` across multi-turn conversations, and caches it transparently. No restarts, no config changes — mix thinking and non-thinking models freely in the same session.

## Why this image exists

DeepSeek V4 Pro enables Thinking mode by default and returns `reasoning_content`.  
In multi-turn chat, clients must send that field back on later turns. Cursor currently does not forward `reasoning_content`, which can cause follow-up requests to fail.

This proxy fixes that compatibility issue by forcing `thinking=disabled` by default.

## Features

- OpenAI-compatible `/v1` endpoint for Cursor
- Fixes DeepSeek V4 Pro multi-turn compatibility issues
- Per-request Thinking mode: use `-thinking` suffixed model names to enable reasoning for specific conversations
- Automatically bridges `reasoning_content` across turns when Thinking mode is active (non-streaming)
- Transparently forwards your own DeepSeek API key — no server-side keys needed
- Lightweight multi-stage Docker image
- Customizable upstream, model mapping
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

## Environment Variables

- `UPSTREAM` - Upstream endpoint (default: `https://api.deepseek.com`)
- `LISTEN` - Listen address (default: `:8080`)
- `MAPPED_MODEL` - Default mapped model alias target
- `MODEL_MAP` - Extra custom model mappings
- `DS_REASONING_EFFORT` - Reasoning effort for `-thinking` suffix models (`low | medium | high`)
- `DS_CACHE_TTL` - Reasoning cache TTL (default: `24h`)

## Security Notes

- Do **not** bake secrets into images.
- Pass keys using runtime env vars, Docker secrets, or a secret manager.
- If a key was exposed, revoke and rotate it immediately.

## Notes

Cursor usually cannot call localhost directly as Base URL because of SSRF protections.  
Expose this service through a public HTTPS endpoint (ngrok, Cloudflare Tunnel, or reverse proxy).
