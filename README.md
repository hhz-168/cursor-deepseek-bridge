# cursor-deepseek-bridge

[简体中文](./README.zh-CN.md)

A lightweight Go proxy that enables [Cursor](https://cursor.com) to use [DeepSeek V4](https://platform.deepseek.com) models (Pro / Flash) through an OpenAI-compatible endpoint.

> **Highlight: Zero-config Thinking toggle.** Just append `-thinking` to any model name (e.g. `deepseek-v4-pro-thinking`) in Cursor, and the proxy automatically enables reasoning, bridges `reasoning_content` across multi-turn conversations, and caches it transparently. No restarts, no config changes — mix thinking and non-thinking models freely in the same session.

> **New: Image OCR Support.** When Cursor sends an image (e.g. a screenshot), the proxy automatically runs OCR via RapidOCR to extract text and pass it to the DeepSeek API. The AI can then "see" your images.

## Why This Proxy Exists

[DeepSeek V4](https://platform.deepseek.com) has **Thinking mode enabled by default**, meaning every response includes `reasoning_content` (the model's internal reasoning). In multi-turn conversations, the client must echo the previous turn's `reasoning_content` back to the API — otherwise the request will fail.

**Cursor does not preserve `reasoning_content` across turns**, so directly connecting Cursor to the DeepSeek API will cause errors starting from the second message. This issue is **specific to DeepSeek V4** and does not affect other versions.

This proxy intercepts requests and forces `thinking` to `disabled` by default, resolving the compatibility gap between Cursor and DeepSeek V4.

Additionally, Cursor's **Base URL cannot point to a local address** (`127.0.0.1`, `localhost`, etc.) — requests are blocked by Cursor's SSRF protection and return `ssrf_blocked`. The proxy therefore needs its own proxy: you must expose it via ngrok, Cloudflare Tunnel, or a custom domain with Nginx reverse proxy as a public HTTPS URL before Cursor can reach it.

## Quick Start

### Prerequisites

- **Go 1.22+** (for local development)
- **Python 3.10+** with `rapidocr` (for image OCR; Docker image includes this automatically)
- **DeepSeek API Key** from [platform.deepseek.com](https://platform.deepseek.com)

### 1. Get a DeepSeek API Key

Sign up at the [DeepSeek Platform](https://platform.deepseek.com) and create an API key. This key will be configured in Cursor — the proxy transparently forwards your key to the DeepSeek API.

### 2. Start the Proxy

**Option A: Run directly (local)** — requires Python + rapidocr

```bash
# Install OCR dependencies
pip install rapidocr onnxruntime

# Start the proxy
go run main.go
```

**Option B: Docker Compose**

```bash
docker compose up -d --build
```

**Option C: Pull from Docker Hub**

Image: [houzingm/cursor-deepseek-bridge](https://hub.docker.com/r/houzingm/cursor-deepseek-bridge)

```bash
docker run -d \
  --name cursor-deepseek-bridge \
  -p 8080:8080 \
  houzingm/cursor-deepseek-bridge:latest
```

The proxy listens on `0.0.0.0:8080` by default.

### 3. Expose to the Internet

Use a tunneling tool to expose port 8080 as HTTPS:

**ngrok:**

```bash
ngrok http 8080
```

**Cloudflare Tunnel:**

```bash
cloudflared tunnel --url http://localhost:8080
```

You'll get a public URL like `https://xxxx.ngrok-free.app`.

If you have your own domain, you can also use Nginx as a reverse proxy with SSL:

```nginx
server {
    listen 443 ssl;
    server_name your-domain.com;

    ssl_certificate     /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

### 4. Configure Cursor

In Cursor, set the following:

- **OpenAI Base URL**: `https://your-public-url/v1` (the trailing `/v1` is required)
- **API Key**: Your DeepSeek API key from step 1
- **Model**: Choose `deepseek-v4-pro` or `deepseek-v4-flash` (or your custom mapped model name)
  - Append `-thinking` to the model name (e.g. `deepseek-v4-pro-thinking` / `deepseek-v4-flash-thinking`) to enable reasoning for that conversation. Mix thinking and non-thinking models freely in the same session — no proxy restart needed.

## Image OCR

When you send an image in Cursor, the proxy automatically:

1. Extracts the base64-encoded image from the message content
2. Runs OCR via [RapidOCR](https://github.com/RapidAI/RapidOCR) (ONNXRuntime backend)
3. Replaces the `image_url` content with the recognized text (structured with position bounding boxes and confidence scores)

The DeepSeek API then receives the text content, allowing the AI to understand what's in your screenshots.

### Requirements

- **Docker**: OCR is included in the Docker image automatically
- **Local**: Install `rapidocr` and `onnxruntime`:

  ```bash
  pip install rapidocr onnxruntime
  ```

- Specify a custom Python executable path if needed:

  ```bash
  export PYTHON=/path/to/python
  ```

### Related Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PYTHON` | `python` | Path to Python executable (for OCR worker subprocess) |
| `DS_MAX_REQUEST_BODY` | `32m` (32 MiB) | Max HTTP request body size. Images are base64-encoded (~33% larger), so set this higher for hi-res images. Supports `m` (MiB), `g` (GiB), `k` (KiB) suffixes. Min: 1m, Max: 256m |

## Configuration Reference

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `UPSTREAM` | No | `https://api.deepseek.com` | Upstream API endpoint |
| `LISTEN` | No | `:8080` | Proxy listen address |
| `MAPPED_MODEL` | No | `deepseek-v4-pro` | Default target for all unrecognized model names |
| `DS_REASONING_EFFORT` | No | `high` | Reasoning effort when using `-thinking` suffixed models (`low` / `medium` / `high`) |
| `DS_CACHE_TTL` | No | `24h` | TTL for reasoning content hash cache (used by `-thinking` suffixed models to bridge `reasoning_content` across turns) |
| `DS_QUEUE_TTL` | No | `24h` | TTL for per-conversation order queue; idle queues are cleaned after this duration |
| `DS_MAX_REQUEST_BODY` | No | `32m` | Max HTTP request body size (min: 1m, max: 256m). Increase for large images |
| `DS_DEBUG` | No | `false` | Enable debug logging (prints request/response bodies; set to `true`) |
| `PYTHON` | No | `python` | Python executable path for OCR worker subprocess |
| `MODEL_MAP` | No | (built-in) | Extra custom model mappings (format: `alias=real`, comma-separated) |

### Model Mapping

The proxy automatically maps common OpenAI model names used in Cursor to DeepSeek models:

- `gpt-4o` → `deepseek-v4-pro`
- `gpt-4o-mini` → `deepseek-v4-pro`
- `gpt-4` / `gpt-4-turbo` → `deepseek-v4-pro`
- `gpt-3.5-turbo` → `deepseek-v4-pro`
- `chatgpt-4o-latest` → `deepseek-v4-pro`

DeepSeek models pass through directly, including their `-thinking` variants:

- `deepseek-v4-pro` → `deepseek-v4-pro` (thinking disabled)
- `deepseek-v4-pro-thinking` → `deepseek-v4-pro` (thinking enabled)
- `deepseek-v4-flash` → `deepseek-v4-flash` (thinking disabled)
- `deepseek-v4-flash-thinking` → `deepseek-v4-flash` (thinking enabled)

Custom mappings can be configured via environment variables:

```bash
# Change the default mapping target
export MAPPED_MODEL=deepseek-v4-flash

# Add custom mappings (format: alias=real-model, comma-separated)
export MODEL_MAP=claude-3-opus=deepseek-v4-pro,gpt-4o=deepseek-v4-flash
```

### Thinking Mode

DeepSeek V4 has Thinking mode enabled by default, but Cursor does not echo `reasoning_content` in multi-turn conversations, which can cause errors. The proxy therefore sets `thinking` to `disabled` by default for all requests.

To use DeepSeek's reasoning capabilities, select a model with the **`-thinking` suffix** in Cursor (e.g., `deepseek-v4-flash-thinking` instead of `deepseek-v4-flash`). The proxy will:

- Enable `thinking` only for that specific request
- Automatically bridge `reasoning_content` across multi-turn conversations by caching the reasoning from each assistant response and injecting it back into the next request's message history

This per-request approach lets you freely mix thinking and non-thinking models in the same Cursor session without restarting the proxy.

```bash
# Optional: set reasoning effort level (low / medium / high)
export DS_REASONING_EFFORT=high
# Optional: cache TTL for bridging reasoning_content across turns (default: 24h)
export DS_CACHE_TTL=1h
```

> **Note**: Currently only non-streaming (JSON) responses are supported for reasoning caching. Streaming responses will be supported in a future update.

## Health Check

```bash
curl http://localhost:8080/healthz
# Returns: ok
```

## License

[MIT License](LICENSE)

Use of the DeepSeek API is subject to the [DeepSeek Platform](https://platform.deepseek.com) terms of service. This project is not affiliated with DeepSeek or Cursor.
