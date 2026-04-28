#!/usr/bin/env bash
set -eu

IMAGE="${IMAGE:-houzingm/cursor-deepseek-bridge}"
DATE_TAG="$(date -u +%Y%m%d-%H%M%S)"

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "==> Building ${IMAGE}:${DATE_TAG}"
docker build -t "${IMAGE}:${DATE_TAG}" -t "${IMAGE}:latest" "${ROOT_DIR}"

echo ""
echo "==> Pushing to Docker Hub..."
docker push "${IMAGE}:${DATE_TAG}"
docker push "${IMAGE}:latest"

echo ""
echo "==> Done! ${IMAGE}:${DATE_TAG}  (+ :latest)"
