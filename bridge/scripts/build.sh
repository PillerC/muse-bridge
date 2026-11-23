#!/usr/bin/env bash
# 交叉编译，产出单个静态二进制。服务器上不需要任何运行时。
#
# 用法：
#   bash scripts/build.sh                    # 只编当前平台到 dist/
#   GOOS=linux GOARCH=arm64 bash scripts/build.sh
#   ALL=1 bash scripts/build.sh              # 编发布用的全部平台
#
# 版本号从 git tag 推出来（git describe），注入 internal/version。
# 发布流水线（.github/workflows/release.yml）也用这个脚本，保证本地和 CI 一致。
set -euo pipefail

cd "$(dirname "$0")/.."

GO_BIN="${GO_BIN:-go}"
OUT_DIR="${OUT_DIR:-dist}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo '')}"

# 发布用的平台矩阵。资产名必须与 internal/upgrade.AssetName 拼出来的一致，
# 否则升级器找不到对应文件。
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"

mkdir -p "$OUT_DIR"

# build_one <goos> <goarch>：编一个平台，文件名 bridge-<os>-<arch>[.exe]。
build_one() {
  local goos="$1" goarch="$2"
  local name="bridge-${goos}-${goarch}"
  [ "$goos" = "windows" ] && name="${name}.exe"

  echo "==> ${goos}/${goarch} -> ${OUT_DIR}/${name}"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$GO_BIN" build \
    -trimpath \
    -ldflags "-s -w -X muse-bridge/internal/version.Version=${VERSION} -X muse-bridge/internal/version.Commit=${COMMIT}" \
    -o "${OUT_DIR}/${name}" ./cmd/bridge
}

if [ "${ALL:-0}" = "1" ]; then
  for p in $PLATFORMS; do
    build_one "${p%%/*}" "${p##*/}"
  done
  # 校验文件给在线升级用：升级器只认 checksums.txt，缺了就不升级。
  # 用 sha256sum 的格式（<hash>  <name>），两空格分隔。
  ( cd "$OUT_DIR" && sha256sum bridge-* > checksums.txt )
  echo "--- ${OUT_DIR}/checksums.txt"
  cat "${OUT_DIR}/checksums.txt"
else
  build_one "${GOOS:-linux}" "${GOARCH:-amd64}"
fi

ls -lh "$OUT_DIR" | tail -n +2
echo "版本: $VERSION（${COMMIT:-无提交信息}）"
