# cursor-deepseek-bridge

一个轻量级 Go 代理，让 [Cursor](https://cursor.com) 通过 OpenAI 兼容接口使用 [DeepSeek V4 Pro](https://platform.deepseek.com) 模型。

## 为什么需要这个代理

[DeepSeek V4](https://platform.deepseek.com) 默认开启 **Thinking 模式**，每次响应都会包含 `reasoning_content`（推理过程）。但在多轮对话中，客户端需要将上一轮的 `reasoning_content` 原样带回给 API，否则请求会失败。

**Cursor 在多轮对话中不会回传 `reasoning_content`**，因此直接对接 DeepSeek API 会导致第二轮及之后的对话报错——这是 **仅针对 DeepSeek V4 的问题**，其他版本不受影响。

这个代理在中间层拦截请求，默认将 `thinking` 参数强制设为 `disabled`，从而解决 Cursor 与 DeepSeek V4 的兼容性问题。

另外，Cursor 的 **Base URL 不能直接使用本地地址**（`127.0.0.1`、`localhost` 等），请求会被 Cursor 侧的 SSRF 防护拦截，返回 `ssrf_blocked`，因此代理本身也需要代理——通过 ngrok、Cloudflare Tunnel 或自有域名 + Nginx 反向代理暴露为公网 HTTPS 地址后才能被 Cursor 访问。

## 快速开始

### 1. 获取 DeepSeek API Key

前往 [DeepSeek 平台](https://platform.deepseek.com) 注册并获取 API Key。

### 2. 启动代理

**方式一：直接运行**

```bash
export DEEPSEEK_API_KEY=sk-你的key
go run main.go
```

**方式二：Docker Compose**

```bash
# 创建 .env 文件
echo "DEEPSEEK_API_KEY=sk-你的key" > .env

# 启动
docker compose up -d --build
```

代理默认监听 `0.0.0.0:8080`。

### 3. 暴露到公网

使用你喜欢的隧道工具将本地 8080 端口暴露为 HTTPS：

**ngrok：**
```bash
ngrok http 8080
```

**Cloudflare Tunnel：**
```bash
cloudflared tunnel --url http://localhost:8080
```

你会得到一个类似 `https://xxxx.ngrok-free.app` 的公网地址。

如果你有自己的域名，也可以用 Nginx 反向代理并配置 SSL 证书：

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

### 4. 配置 Cursor

在 Cursor 中进行以下设置：

- **OpenAI Base URL**：`https://你的公网地址/v1`（注意结尾必须有 `/v1`）
- **API Key**：如果设置了 `PROXY_API_KEY`，填写该值；否则可为任意非空字符串
- **Model**：选择 `deepseek-v4-pro`（或你自定义映射的模型名）

## 配置说明

### 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `DEEPSEEK_API_KEY` | 是 | - | DeepSeek 平台的 API Key |
| `UPSTREAM` | 否 | `https://api.deepseek.com` | 上游 API 地址 |
| `LISTEN` | 否 | `:8080` | 代理监听地址 |
| `PROXY_API_KEY` | 否 | 无 | 对外暴露时的认证密钥，设置后 Cursor 中的 API Key 需填写相同值 |
| `MAPPED_MODEL` | 否 | `deepseek-v4-pro` | 所有未知模型名的默认映射目标 |

### 模型映射

代理会自动将 Cursor 中常见的 OpenAI 模型名映射为 DeepSeek 模型：

- `gpt-4o` → `deepseek-v4-pro`
- `gpt-4o-mini` → `deepseek-v4-pro`
- `gpt-4` / `gpt-4-turbo` → `deepseek-v4-pro`
- `gpt-3.5-turbo` → `deepseek-v4-pro`
- `chatgpt-4o-latest` → `deepseek-v4-pro`

可通过环境变量自定义：

```bash
# 修改默认映射目标
export MAPPED_MODEL=deepseek-v4-flash

# 添加额外映射（格式：别名=真实模型名，逗号分隔）
export MODEL_MAP=claude-3-opus=deepseek-v4-pro,gpt-4o=deepseek-v4-flash
```

### Thinking 模式

DeepSeek V4 默认开启 Thinking 模式，但 Cursor 在多轮对话中不会回传 `reasoning_content`，可能导致异常。因此代理默认将 thinking 设为 `disabled`。

如需开启：

```bash
export DS_THINKING=enabled
# 可选：设置推理深度（low / medium / high）
export DS_REASONING_EFFORT=high
```

## 健康检查

```bash
curl http://localhost:8080/healthz
# 返回: ok
```

## 许可证

[MIT License](LICENSE)

使用 DeepSeek API 须遵守 [DeepSeek 平台](https://platform.deepseek.com) 的使用规范。本项目与 DeepSeek、Cursor 无附属关系。
