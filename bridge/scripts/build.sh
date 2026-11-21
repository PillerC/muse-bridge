#!/usr/bin/env bash
# 本地交叉编译，产出单个静态二进制。服务器上不需要任何运行时。
set -euo pipefail

cd "$(dirname "$0")/.."

GO_BIN="${GO_BIN:-go}"
OUT_DIR="${OUT_DIR:-dist}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)}"

mkdir -p "$OUT_DIR"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO_BIN" build \
  -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o "$OUT_DIR/bridge-linux-amd64" ./cmd/bridge

ls -lh "$OUT_DIR/bridge-linux-amd64"
echo "版本: $VERSION"
