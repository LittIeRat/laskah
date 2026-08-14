#!/usr/bin/env bash
# 在 Linux 上编译 Laskah。需要 Go >= 1.26（见 go.mod）。
#
#   bash scripts/build.sh              # 编译当前架构
#   TARGETS="linux/amd64 linux/arm64" bash scripts/build.sh
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p bin

if ! command -v go >/dev/null 2>&1; then
  echo "找不到 go，请先安装 Go 1.26 或更新版本" >&2
  exit 1
fi
echo "工具链: $(go version)"

TARGETS="${TARGETS:-}"
if [ -z "$TARGETS" ]; then
  TARGETS="$(go env GOOS)/$(go env GOARCH)"
fi

for target in $TARGETS; do
  goos="${target%%/*}"
  goarch="${target##*/}"
  out="bin/laskah-${goos}-${goarch}"
  echo "编译 ${goos}/${goarch} -> $out"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags '-s -w' -o "$out" ./cmd/laskah
done

echo
ls -l bin/
echo
echo "SHA256:"
sha256sum bin/* 2>/dev/null || shasum -a 256 bin/*
