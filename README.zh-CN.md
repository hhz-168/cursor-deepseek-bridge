# cursor-deepseek-bridge

[English](./README.md)

一个轻量级 Go 代理，让 [Cursor](https://cursor.com) 通过 OpenAI 兼容接口使用 [DeepSeek V4](https://platform.deepseek.com) 模型（Pro / Flash）。

> **亮点：零配置 Thinking 开关。** 在 Cursor 中，只需在模型名后加上 `-thinking` 后缀（如 `deepseek-v4-pro-thinking`），代理就会自动开启推理模式，并在多轮对话间透明桥接 `reasoning_content`，自动缓存。无需重启、无需改配置——在同一会话中自由混用 thinking 和非 thinking 模型。

> **新功能：图片 OCR 支持。** 当 Cursor 发送图片（如截图）时，代理会自动通过 RapidOCR 进行文字识别，提取出的文字会传递给 DeepSeek API，让 AI"看懂"你的图片。

## 为什么需要这个代理

[DeepSeek V4](https://platform.deepseek.com) 默认开启 **Thinking 模式**，每次响应都会包含 `reasoning_content`（推理过程）。但在多轮对话中，客户端需要将上一轮的 `reasoning_content` 原样带回给 API，否则请求会失败。

**Cursor 在多轮对话中不会回传 `reasoning_content`**，因此直接对接 DeepSeek API 会导致第二轮及之后的对话报错——这是 **仅针对 DeepSeek V4 的问题**，其他版本不受影响。

这个代理在中间层拦截请求，默认将 `thinking` 参数强制设为 `disabled`，从而解决 Cursor 与 DeepSeek V4 的兼容性问题。

另外，Cursor 的 **Base URL 不能直接使用本地地址**（`127.0.0.1`、`localhost` 等），请求会被 Cursor 侧的 SSRF 防护拦截，返回 `ssrf_blocked`，因此代理本身也需要代理——通过 ngrok、Cloudflare Tunnel 或自有域名 + Nginx 反向代理暴露为公网 HTTPS 地址后才能被 Cursor 访问。

## 快速开始

### 环境要求

- **Go 1.22+**（本地开发）
- **Python 3.10+** 及 `rapidocr`（图片 OCR 功能需要；Docker 镜像已自动包含）
- **DeepSeek API Key** 从 [platform.deepseek.com](https://platform.deepseek.com) 获取

### 1. 获取 DeepSeek API Key

前往 [DeepSeek 平台](https://platform.deepseek.com) 注册并获取 API Key。此 Key 将在 Cursor 中配置——代理会透明地将你的 Key 转发给 DeepSeek API。

### 2. 启动代理

**方式一：直接运行（本地）** — 需安装 Python + rapidocr

```bash
# 安装 OCR 依赖
pip install rapidocr onnxruntime

# 启动代理
go run main.go
```

**方式二：Docker Compose**

```bash
docker compose up -d --build
```

**方式三：直接从 Docker Hub 拉取**

镜像地址：[houzingm/cursor-deepseek-bridge](https://hub.docker.com/r/houzingm/cursor-deepseek-bridge)

```bash
docker run -d \
  --name cursor-deepseek-bridge \
  -p 8080:8080 \
  houzingm/cursor-deepseek-bridge:latest
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
- **API Key**：填写你在 DeepSeek 平台申请的 API Key
- **Model**：选择 `deepseek-v4-pro` 或 `deepseek-v4-flash`（或你自定义映射的模型名）
  - 在模型名后加 `-thinking` 后缀（如 `deepseek-v4-pro-thinking` / `deepseek-v4-flash-thinking`）即可对该对话启用推理。同一会话中可自由混用 thinking 和非 thinking 模型，无需重启代理。

## 图片 OCR

当你在 Cursor 中发送图片时，代理会自动：

1. 从消息内容中提取 base64 编码的图片
2. 使用 [RapidOCR](https://github.com/RapidAI/RapidOCR)（ONNXRuntime 后端）进行文字识别
3. 将 `image_url` 内容替换为识别出的文字（包含位置边界框和置信度分数）

DeepSeek API 随后会收到文字内容，让 AI 能够理解你的截图内容。

### 环境要求

- **Docker**：OCR 功能已内置在 Docker 镜像中，无需额外配置
- **本地运行**：需要安装 `rapidocr` 和 `onnxruntime`：

  ```bash
  pip install rapidocr onnxruntime
  ```

- 如需指定 Python 解释器路径：

  ```bash
  export PYTHON=/path/to/python
  ```

### 相关环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PYTHON` | `python` | Python 解释器路径（用于 OCR worker 子进程） |
| `DS_MAX_REQUEST_BODY` | `32m`（32 MiB） | HTTP 请求体最大大小。图片经过 base64 编码后会膨胀约 33%，发大图时请调大此值。支持 `m`（MiB）、`g`（GiB）、`k`（KiB）后缀。最小：1m，最大：256m |

## 配置说明

### 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `UPSTREAM` | 否 | `https://api.deepseek.com` | 上游 API 地址 |
| `LISTEN` | 否 | `:8080` | 代理监听地址 |
| `MAPPED_MODEL` | 否 | `deepseek-v4-pro` | 所有未知模型名的默认映射目标 |
| `DS_REASONING_EFFORT` | 否 | `high` | 使用 `-thinking` 后缀模型时的推理深度（`low` / `medium` / `high`） |
| `DS_CACHE_TTL` | 否 | `24h` | reasoning 内容哈希缓存过期时间（用于 `-thinking` 后缀模型跨轮桥接 `reasoning_content`） |
| `DS_QUEUE_TTL` | 否 | `24h` | 对话顺序队列过期时间，空闲超时的对话队列会被自动清理 |
| `DS_MAX_REQUEST_BODY` | 否 | `32m` | HTTP 请求体最大大小（最小 1m，最大 256m）。发大图时可调大 |
| `DS_DEBUG` | 否 | `false` | 开启调试日志（输出请求/响应体；设为 `true`） |
| `PYTHON` | 否 | `python` | Python 解释器路径（用于 OCR worker 子进程） |
| `MODEL_MAP` | 否 | （内置映射） | 额外自定义模型映射（格式：`别名=真实模型名`，逗号分隔） |

### 模型映射

代理会自动将 Cursor 中常见的 OpenAI 模型名映射为 DeepSeek 模型：

- `gpt-4o` → `deepseek-v4-pro`
- `gpt-4o-mini` → `deepseek-v4-pro`
- `gpt-4` / `gpt-4-turbo` → `deepseek-v4-pro`
- `gpt-3.5-turbo` → `deepseek-v4-pro`
- `chatgpt-4o-latest` → `deepseek-v4-pro`

DeepSeek 原生模型直接透传，包括 `-thinking` 变体：

- `deepseek-v4-pro` → `deepseek-v4-pro`（thinking 禁用）
- `deepseek-v4-pro-thinking` → `deepseek-v4-pro`（thinking 启用）
- `deepseek-v4-flash` → `deepseek-v4-flash`（thinking 禁用）
- `deepseek-v4-flash-thinking` → `deepseek-v4-flash`（thinking 启用）

可通过环境变量自定义：

```bash
# 修改默认映射目标
export MAPPED_MODEL=deepseek-v4-flash

# 添加额外映射（格式：别名=真实模型名，逗号分隔）
export MODEL_MAP=claude-3-opus=deepseek-v4-pro,gpt-4o=deepseek-v4-flash
```

### Thinking 模式

DeepSeek V4 默认开启 Thinking 模式，但 Cursor 在多轮对话中不会回传 `reasoning_content`，可能导致异常。因此代理默认将 `thinking` 设为 `disabled`。

如需使用 DeepSeek 的推理能力，在 Cursor 中选择带有 **`-thinking` 后缀**的模型（例如使用 `deepseek-v4-flash-thinking` 替代 `deepseek-v4-flash`）。代理会：

- 仅对该请求启用 `thinking`
- 自动在多轮对话中桥接 `reasoning_content`：缓存每个 assistant 回复中的推理内容，并在下一次请求的消息历史中自动补回

这种按请求控制的方式让你可以在同一个 Cursor 会话中自由混用 thinking 和非 thinking 模型，无需重启代理。

```bash
# 可选：设置推理深度（low / medium / high）
export DS_REASONING_EFFORT=high
# 可选：设置 reasoning 缓存过期时间（默认 24h）
export DS_CACHE_TTL=24h
```

> **注意**：目前仅非流式（JSON）响应支持 reasoning 缓存，流式响应将在后续更新中支持。

## 健康检查

```bash
curl http://localhost:8080/healthz
# 返回: ok
```

## 许可证

[MIT License](LICENSE)

使用 DeepSeek API 须遵守 [DeepSeek 平台](https://platform.deepseek.com) 的使用规范。本项目与 DeepSeek、Cursor 无附属关系。
