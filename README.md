# cursor-deepseek-bridge

A lightweight Go proxy that enables [Cursor](https://cursor.com) to use [DeepSeek V4 Pro](https://platform.deepseek.com) through an OpenAI-compatible endpoint.

## Why This Proxy Exists

[DeepSeek V4](https://platform.deepseek.com) has **Thinking mode enabled by default**, meaning every response includes `reasoning_content` (the model's internal reasoning). In multi-turn conversations, the client must echo the previous turn's `reasoning_content` back to the API — otherwise the request will fail.

**Cursor does not preserve `reasoning_content` across turns**, so directly connecting Cursor to the DeepSeek API will cause errors starting from the second message. This issue is **specific to DeepSeek V4** and does not affect other versions.

This proxy intercepts requests and forces `thinking` to `disabled` by default, resolving the compatibility gap between Cursor and DeepSeek V4.

Additionally, Cursor's **Base URL cannot point to a local address** (`127.0.0.1`, `localhost`, etc.) — requests are blocked by Cursor's SSRF protection and return `ssrf_blocked`. The proxy therefore needs its own proxy: you must expose it via ngrok, Cloudflare Tunnel, or a custom domain with Nginx reverse proxy as a public HTTPS URL before Cursor can reach it.

## Quick Start

### 1. Get a DeepSeek API Key

Sign up at the [DeepSeek Platform](https://platform.deepseek.com) and create an API key.

### 2. Start the Proxy

**Option A: Run directly**

```bash
export DEEPSEEK_API_KEY=sk-your-key
go run main.go
```

**Option B: Docker Compose**

```bash
# Create a .env file
echo "DEEPSEEK_API_KEY=sk-your-key" > .env

# Start
docker compose up -d --build
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
- **API Key**: If `PROXY_API_KEY` is set, use that value; otherwise any non-empty string works
- **Model**: Choose `deepseek-v4-pro` (or your custom mapped model name)

## Configuration Reference

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DEEPSEEK_API_KEY` | Yes | - | Your DeepSeek platform API key |
| `UPSTREAM` | No | `https://api.deepseek.com` | Upstream API endpoint |
| `LISTEN` | No | `:8080` | Proxy listen address |
| `PROXY_API_KEY` | No | (none) | Authentication key for public exposure; Cursor API Key must match |
| `MAPPED_MODEL` | No | `deepseek-v4-pro` | Default target for all unrecognized model names |

### Model Mapping

The proxy automatically maps common OpenAI model names used in Cursor to DeepSeek models:

- `gpt-4o` → `deepseek-v4-pro`
- `gpt-4o-mini` → `deepseek-v4-pro`
- `gpt-4` / `gpt-4-turbo` → `deepseek-v4-pro`
- `gpt-3.5-turbo` → `deepseek-v4-pro`
- `chatgpt-4o-latest` → `deepseek-v4-pro`

Custom mappings can be configured via environment variables:

```bash
# Change the default mapping target
export MAPPED_MODEL=deepseek-v4-flash

# Add custom mappings (format: alias=real-model, comma-separated)
export MODEL_MAP=claude-3-opus=deepseek-v4-pro,gpt-4o=deepseek-v4-flash
```

### Thinking Mode

DeepSeek V4 has Thinking mode enabled by default, but Cursor does not echo `reasoning_content` in multi-turn conversations, which can cause errors. The proxy therefore sets thinking to `disabled` by default.

To enable it:

```bash
export DS_THINKING=enabled
# Optional: set reasoning effort level (low / medium / high)
export DS_REASONING_EFFORT=high
```

## Health Check

```bash
curl http://localhost:8080/healthz
# Returns: ok
```

## License

[MIT License](LICENSE)

Use of the DeepSeek API is subject to the [DeepSeek Platform](https://platform.deepseek.com) terms of service. This project is not affiliated with DeepSeek or Cursor.
