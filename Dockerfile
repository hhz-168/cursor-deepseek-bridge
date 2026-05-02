FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxy .

# glibc base: onnxruntime has no usable musllinux wheels for Alpine
FROM python:3.12-slim-bookworm AS ocr-runtime
RUN pip install --no-cache-dir rapidocr onnxruntime

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libgomp1 \
    libglib2.0-0 \
    libxcb1 \
    libx11-6 \
    libxext6 \
    libsm6 \
    libgl1 \
    && rm -rf /var/lib/apt/lists/*
# Copy Go proxy binary
COPY --from=build /out/proxy /proxy
# Copy Python OCR worker
COPY ocr_worker.py /ocr_worker.py
# Copy Python runtime + site-packages (including rapidocr + onnxruntime)
COPY --from=ocr-runtime /usr/local/ /usr/local/

# Default Python executable path inside container
ENV PYTHON=python3
ENV LISTEN=:8080
EXPOSE 8080
USER nobody
ENTRYPOINT ["/proxy"]
