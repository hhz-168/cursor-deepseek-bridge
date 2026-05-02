# cursor-deepseek-bridge

[简体中文](./README.zh-CN.md)

A lightweight Go proxy that enables [Cursor](https://cursor.com) to use [DeepSeek V4](https://platform.deepseek.com) models (Pro / Flash) through an OpenAI-compatible endpoint.

**Thinking mode toggle** — append `-thinking` to any model name (e.g. `deepseek-v4-pro-thinking`) in Cursor, and the proxy transparently enables reasoning, bridges `reasoning_content` across multi-turn conversations, and caches it. Mix thinking and non-thinking models freely in the same session.

**Image OCR** — when Cursor sends an image, the proxy runs OCR via RapidOCR to extract text and pass it to DeepSeek API. The AI can then "see" your screenshots.

![1777705403407](image/README.zh-CN/1777705403407.png)

---

## The Problem

DeepSeek V4 enables Thinking mode by default and returns `reasoning_content`. In multi-turn conversations, clients must echo the previous turn's `reasoning_content` back to the API — otherwise the request fails. **Cursor does not preserve `reasoning_content` across turns**, so directly connecting Cursor to DeepSeek API will cause errors starting from the second message.

This proxy intercepts requests and sets `thinking` to `disabled` by default, resolving the compatibility gap. Additionally, Cursor's SSRF protection blocks `localhost` / `127.0.0.1` as Base URL, so the proxy must be exposed via a public HTTPS endpoint (see below).

---

## Quick Start

### 1. Get a DeepSeek API Key

Sign up at [platform.deepseek.com](https://platform.deepseek.com) and create an API key. This key will be configured in Cursor — the proxy transparently forwards it to DeepSeek.

### 2. Start the Proxy

**Option A: Local (Go + Python)**

```bash
pip install rapidocr onnxruntime
go run main.go
```

**Option B: Docker Compose**

```bash
docker compose up -d --build
```

**Option C: Docker Hub**

```bash
docker run -d --name cursor-deepseek-bridge -p 8080:8080 houzingm/cursor-deepseek-bridge:latest
```

The proxy listens on `0.0.0.0:8080` by default.

### 3. Expose to the Internet

Use a tunneling tool to expose port 8080 as HTTPS — Cursor cannot reach local addresses directly.

```bash
# Option A: ngrok
ngrok http 8080

# Option B: Cloudflare Tunnel
cloudflared tunnel --url http://localhost:8080
```

Or use Nginx reverse proxy with SSL on your own domain:

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

| Setting                   | Value                                                      |
| ------------------------- | ---------------------------------------------------------- |
| **OpenAI Base URL** | `https://your-public-url/v1` (trailing `/v1` required) |
| **API Key**         | Your DeepSeek API key                                      |
| **Model**           | `deepseek-v4-pro` or `deepseek-v4-flash`               |

Append `-thinking` to the model name (e.g. `deepseek-v4-pro-thinking`) to enable reasoning for that conversation.

---

## Image OCR

When you send an image in Cursor, the proxy automatically:

1. Extracts the base64-encoded image from the message content
2. Runs OCR via [RapidOCR](https://github.com/RapidAI/RapidOCR) (ONNXRuntime backend)
3. Replaces `image_url` content with recognized text (with position bounding boxes and confidence scores)

**Requirements:**

- **Docker**: OCR is pre-installed in the Docker image
- **Local**: Install `rapidocr` and `onnxruntime` (`pip install rapidocr onnxruntime`)
- Custom Python path: `export PYTHON=/path/to/python`

---

## Advanced Configuration

### Environment Variables

| Variable                | Default                      | Description                                                                 |
| ----------------------- | ---------------------------- | --------------------------------------------------------------------------- |
| `UPSTREAM`            | `https://api.deepseek.com` | Upstream API endpoint                                                       |
| `LISTEN`              | `:8080`                    | Proxy listen address                                                        |
| `MAPPED_MODEL`        | `deepseek-v4-pro`          | Default target for unknown model names                                      |
| `MODEL_MAP`           | (built-in)                   | Extra model mappings (`alias=real`, comma-separated)                      |
| `DS_REASONING_EFFORT` | `high`                     | Reasoning effort for `-thinking` models (`low` / `medium` / `high`) |
| `DS_CACHE_TTL`        | `24h`                      | Reasoning content cache TTL                                                 |
| `DS_QUEUE_TTL`        | `24h`                      | Conversation order queue TTL                                                |
| `DS_MAX_REQUEST_BODY` | `32m`                      | Max request body size (min `1m`, max `256m`). Increase for large images |
| `DS_DEBUG`            | `false`                    | Enable debug logging (set to `true`)                                      |
| `PYTHON`              | `python`                   | Python executable path (for OCR worker)                                     |

### Model Mapping

The proxy maps common OpenAI model names to DeepSeek models:

| Cursor Model                                                                                           | Target                                              |
| ------------------------------------------------------------------------------------------------------ | --------------------------------------------------- |
| `gpt-4o` / `gpt-4o-mini` / `gpt-4` / `gpt-4-turbo` / `gpt-3.5-turbo` / `chatgpt-4o-latest` | `deepseek-v4-pro`                                 |
| `deepseek-v4-pro` / `deepseek-v4-pro-thinking`                                                     | `deepseek-v4-pro` (thinking disabled / enabled)   |
| `deepseek-v4-flash` / `deepseek-v4-flash-thinking`                                                 | `deepseek-v4-flash` (thinking disabled / enabled) |

Custom mappings:

```bash
export MAPPED_MODEL=deepseek-v4-flash
export MODEL_MAP=claude-3-opus=deepseek-v4-pro,gpt-4o=deepseek-v4-flash
```

### Thinking Mode Details

DeepSeek V4 enables Thinking mode by default. The proxy sets `thinking` to `disabled` for all requests by default. To enable reasoning:

- Use a model with `-thinking` suffix — the proxy enables `thinking` only for that request
- `reasoning_content` is cached and bridged across multi-turn conversations automatically
- Currently only non-streaming (JSON) responses support reasoning caching

```bash
# Optional: set reasoning effort
export DS_REASONING_EFFORT=high
# Optional: set cache TTL
export DS_CACHE_TTL=1h
```

### Health Check

```bash
curl http://localhost:8080/healthz
# Returns: ok
```

---

## License

[MIT License](LICENSE)

Use of DeepSeek API is subject to [DeepSeek Platform](https://platform.deepseek.com) terms of service. This project is not affiliated with DeepSeek or Cursor.
