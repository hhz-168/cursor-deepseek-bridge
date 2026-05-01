FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxy .

FROM python:3.12-alpine AS ocr-runtime
# Install RapidOCR with ONNX Runtime support
RUN pip install --no-cache-dir rapidocr onnxruntime

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
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
