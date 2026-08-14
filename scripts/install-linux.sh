#!/usr/bin/env bash
# Laskah 一键安装 / 升级脚本（Debian、Ubuntu、CentOS、Rocky 等 systemd 发行版）
#
# 用法（在解压出来的源码目录里，用 root 或 sudo 执行）：
#
#   sudo bash scripts/install-linux.sh                    # 需要已装好 Go
#   sudo LASKAH_AUTO_GO=1 bash scripts/install-linux.sh   # 顺带自动装 Go
#
# 做的事：装 Go（可选）-> 编译二进制 -> 建系统用户与目录 -> 生成 env（含随机密钥）
#         -> 装 systemd 单元 -> 启动 -> 健康检查
#
# 环境变量 LASKAH_AUTO_GO=1 时，缺少 Go 或版本过低会自动下载官方压缩包装到 /usr/local/go。
# 重复执行是安全的：已存在的 /etc/laskah/laskah.env 与 /var/lib/laskah/db.json 不会被覆盖。
set -euo pipefail

BIN_DIR=/opt/laskah
DATA_DIR=/var/lib/laskah
ETC_DIR=/etc/laskah
ENV_FILE="$ETC_DIR/laskah.env"
SERVICE=/etc/systemd/system/laskah.service
SVC_USER=laskah

if [ "$(id -u)" -ne 0 ]; then
  echo "请用 root 或 sudo 运行" >&2
  exit 1
fi

cd "$(dirname "$0")/.."
SRC="$(pwd)"
echo "==> 源码目录: $SRC"

if [ -d "$SRC/data" ]; then
  echo "警告: 源码目录里出现了 data/，那是别人机器上的数据，建议删掉再装" >&2
fi

# ---------- 1. Go 工具链 ----------
# 已装且版本够用就直接用；否则在 LASKAH_AUTO_GO=1 时自动装官方压缩包到 /usr/local/go。
# 版本号从 https://go.dev/VERSION?m=text 取当前 stable，不写死。
export PATH="/usr/local/go/bin:$PATH"
GO_MIN="$(awk '/^go /{print $2; exit}' go.mod)"
GO_MIN="${GO_MIN:-1.26}"

go_version_ok() {
  command -v go >/dev/null 2>&1 || return 1
  local have want
  have="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  [ -n "$have" ] || return 1
  want="$GO_MIN"
  # 取两者最小版本，若最小者就是 want 说明 have >= want
  [ "$(printf '%s\n%s\n' "$have" "$want" | sort -V | head -n1)" = "$want" ]
}

install_go() {
  local arch tarball ver url tmp
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    armv6l|armv7l) arch=armv6l ;;
    *) echo "不支持的 CPU 架构: $(uname -m)" >&2; return 1 ;;
  esac
  ver="$(curl -fsSL https://go.dev/VERSION?m=text 2>/dev/null | head -n1)"
  case "$ver" in go*) ;; *) ver="go${GO_MIN}.0" ;; esac
  tarball="${ver}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/$tarball"
  tmp="$(mktemp -d)"
  echo "==> 下载 $url"
  curl -fsSL --retry 3 -o "$tmp/$tarball" "$url"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmp/$tarball"
  rm -rf "$tmp"
  hash -r 2>/dev/null || true
}

if ! go_version_ok; then
  if [ "${LASKAH_AUTO_GO:-0}" = "1" ]; then
    echo "==> 未找到符合要求的 Go（需要 >= $GO_MIN），自动安装"
    install_go
  else
    cat >&2 <<EOF
找不到符合要求的 Go（需要 >= $GO_MIN）。两种解决办法：

  # A. 让脚本自动装（下载官方压缩包到 /usr/local/go）
  sudo LASKAH_AUTO_GO=1 bash scripts/install-linux.sh

  # B. 自己装，版本号见 https://go.dev/dl/
  curl -fsSLO https://go.dev/dl/go1.26.0.linux-amd64.tar.gz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.0.linux-amd64.tar.gz
  export PATH=/usr/local/go/bin:\$PATH

发行版仓库里的 golang-go 往往低于 $GO_MIN，装了也编不过。
EOF
    exit 1
  fi
fi
go_version_ok || { echo "Go 安装后版本仍不满足 >= $GO_MIN" >&2; exit 1; }
echo "==> 工具链: $(go version)"

# 让 go build 有可写的缓存目录（root 下 HOME 可能不可写或未设）。
export HOME="${HOME:-/root}"
export GOCACHE="${GOCACHE:-/var/cache/laskah-go/build}"
export GOMODCACHE="${GOMODCACHE:-/var/cache/laskah-go/mod}"
mkdir -p "$GOCACHE" "$GOMODCACHE"

# ---------- 2. 编译 ----------

GOARCH_NATIVE="$(go env GOARCH)"
OUT="$SRC/bin/laskah-linux-$GOARCH_NATIVE"
mkdir -p "$SRC/bin"
echo "==> 编译 linux/$GOARCH_NATIVE"
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_NATIVE" \
  go build -trimpath -ldflags '-s -w' -o "$OUT" ./cmd/laskah
ls -l "$OUT"

# ---------- 3. 用户与目录 ----------
if ! id -u "$SVC_USER" >/dev/null 2>&1; then
  echo "==> 创建系统用户 $SVC_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER" 2>/dev/null \
    || useradd --system --no-create-home --shell /sbin/nologin "$SVC_USER"
fi
install -d -o root -g root -m 0755 "$BIN_DIR"
install -d -o "$SVC_USER" -g "$SVC_USER" -m 0700 "$DATA_DIR"
install -d -o root -g root -m 0750 "$ETC_DIR"

# ---------- 4. 安装二进制（先停服务，避免 text file busy） ----------
systemctl stop laskah 2>/dev/null || true
install -o root -g root -m 0755 "$OUT" "$BIN_DIR/laskah"
install -o root -g root -m 0644 "$SRC/deploy/DEPLOY.md" "$BIN_DIR/DEPLOY.md"
install -o root -g root -m 0644 "$SRC/deploy/QUICKSTART-LINUX.md" "$BIN_DIR/QUICKSTART-LINUX.md"

# ---------- 5. env 配置 ----------
if [ -f "$ENV_FILE" ]; then
  echo "==> 保留已有 $ENV_FILE"
else
  echo "==> 生成 $ENV_FILE（随机密钥）"
  MASTER_KEY="$(head -c 48 /dev/urandom | base64 -w0 2>/dev/null || head -c 48 /dev/urandom | base64)"
  ADMIN_TOKEN="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  umask 077
  cat > "$ENV_FILE" <<EOF
# 由 scripts/install-linux.sh 生成于 $(date -Is)
HOST=127.0.0.1
PORT=8787
DATA_FILE=$DATA_DIR/db.json

# 数据加密主密钥。丢了等于所有已存凭据无法解密，请备份。
MASTER_KEY=$MASTER_KEY

# 管理 API 的 Bearer 令牌，等价超级管理员权限。
ADMIN_TOKEN=$ADMIN_TOKEN

# 换成自己的域名；用 IP 直连可临时写 http://服务器IP
ALLOW_ORIGIN=https://laskah.example.com

# 反向代理后必须为 true，否则登录限速会把所有访客算作同一个 IP。
TRUST_PROXY=true

STRATEGY=weighted-random
MAX_RETRIES=3
COOLDOWN_MS=30000
FAILURE_THRESHOLD=3
BALANCE_INTERVAL_MS=60000
EOF
  chown root:root "$ENV_FILE"
  chmod 600 "$ENV_FILE"
fi

# ---------- 6. systemd ----------
echo "==> 安装 systemd 单元"
install -o root -g root -m 0644 "$SRC/deploy/laskah.service" "$SERVICE"
systemctl daemon-reload
systemctl enable laskah >/dev/null 2>&1 || true
systemctl restart laskah

# ---------- 7. 健康检查 ----------
echo "==> 等待就绪"
ok=0
for _ in $(seq 1 30); do
  sleep 0.5
  if curl -fsS http://127.0.0.1:8787/health >/dev/null 2>&1; then ok=1; break; fi
done
if [ "$ok" -ne 1 ]; then
  echo "启动失败，最近日志：" >&2
  journalctl -u laskah -n 40 --no-pager >&2 || true
  exit 1
fi

echo
echo "健康检查: $(curl -fsS http://127.0.0.1:8787/health)"
cat <<EOF

安装完成。

下一步：
  1. 配好反向代理与 HTTPS（deploy/nginx-laskah.conf 或 deploy/Caddyfile），
     并把 $ENV_FILE 里的 ALLOW_ORIGIN 改成自己的域名，然后
     sudo systemctl restart laskah
  2. 浏览器打开 https://你的域名/setup 创建超级管理员，
     账号密码请立刻存进密码管理器 —— 服务端加密存储，无法找回。
  3. 备份 $ENV_FILE 里的 MASTER_KEY，它是解密 db.json 的唯一钥匙。

常用命令：
  sudo systemctl status laskah --no-pager
  sudo journalctl -u laskah -f
  sudo systemctl restart laskah
EOF
