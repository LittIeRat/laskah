#!/usr/bin/env bash
# Laskah 远程一键部署：从 GitHub 克隆源码并完成安装。
#
#   curl -fsSL https://raw.githubusercontent.com/LittIeRat/laskah/main/scripts/deploy-from-github.sh | sudo bash
#
# 可用环境变量：
#   LASKAH_REPO    仓库地址，默认 https://github.com/LittIeRat/laskah.git
#   LASKAH_BRANCH  分支，默认 main
#   LASKAH_SRC     源码检出目录，默认 /opt/laskah/src
#
# 幂等：已克隆则改为拉取更新；已有的 /etc/laskah/laskah.env 与 /var/lib/laskah/db.json 不会被覆盖。
# 所以本脚本同时也是升级脚本。
set -euo pipefail

REPO="${LASKAH_REPO:-https://github.com/LittIeRat/laskah.git}"
BRANCH="${LASKAH_BRANCH:-main}"
SRC="${LASKAH_SRC:-/opt/laskah/src}"

if [ "$(id -u)" -ne 0 ]; then
  echo "请用 root 或 sudo 运行" >&2
  exit 1
fi

say() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

# ---------- 1. 基础工具 ----------
need_pkgs=()
command -v git  >/dev/null 2>&1 || need_pkgs+=(git)
command -v curl >/dev/null 2>&1 || need_pkgs+=(curl)
command -v tar  >/dev/null 2>&1 || need_pkgs+=(tar)

if [ ${#need_pkgs[@]} -gt 0 ]; then
  say "安装依赖: ${need_pkgs[*]}"
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq "${need_pkgs[@]}"
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q "${need_pkgs[@]}"
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q "${need_pkgs[@]}"
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache "${need_pkgs[@]}"
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm "${need_pkgs[@]}"
  elif command -v zypper >/dev/null 2>&1; then
    zypper --non-interactive install "${need_pkgs[@]}"
  else
    echo "无法自动安装 ${need_pkgs[*]}，请手动装好后重跑" >&2
    exit 1
  fi
fi

# ---------- 2. 克隆或更新源码 ----------
if [ -d "$SRC/.git" ]; then
  say "更新源码 $SRC"
  git -C "$SRC" remote set-url origin "$REPO"
  git -C "$SRC" fetch --depth 1 origin "$BRANCH"
  git -C "$SRC" checkout -q -B "$BRANCH" "origin/$BRANCH"
  git -C "$SRC" reset -q --hard "origin/$BRANCH"
else
  say "克隆 $REPO ($BRANCH) 到 $SRC"
  mkdir -p "$(dirname "$SRC")"
  rm -rf "$SRC"
  git clone --depth 1 --branch "$BRANCH" "$REPO" "$SRC"
fi
echo "提交: $(git -C "$SRC" rev-parse --short HEAD)  $(git -C "$SRC" log -1 --format=%s)"

# ---------- 3. 交给安装脚本（内含 Go 自动安装、编译、systemd、健康检查） ----------
say "开始安装"
LASKAH_AUTO_GO=1 bash "$SRC/scripts/install-linux.sh"
