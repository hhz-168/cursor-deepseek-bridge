FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxy .

# glibc base: onnxruntime has no usable musllinux wheels for Alpine
FROM python:3.12-slim-bookworm AS ocr-runtime
RUN pip install --no-cache-dir rapidocr onnxruntime
# Pre-download OCR models and set world-readable permissions
RUN python3 -c "from rapidocr import RapidOCR; RapidOCR()" 2>/dev/null || true
# Find and chmod models directory so the final non-root user can write model updates
RUN find /usr/local/lib/python* -path '*/rapidocr/models' -type d -exec chmod -R 777 {} + 2>/dev/null; \
    find /usr/local/lib/python* -path '*/rapidocr/models' -type d -exec echo "models dir: {}" \;

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

# Ensure rapidocr models directory is writable by nobody
RUN find /usr/local/lib/python* -path '*/rapidocr/models' -type d -exec chmod -R 777 {} + 2>/dev/null || true

# Set RAPIDOCR_MODEL_DIR to a writable location as a fallback
ENV RAPIDOCR_MODEL_DIR=/tmp/rapidocr_models

# Default Python executable path inside container
ENV PYTHON=python3
ENV LISTEN=:8080
EXPOSE 8080
USER nobody
ENTRYPOINT ["/proxy"]
