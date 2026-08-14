# Laskah — Linux 部署快速上手

全程约 10 分钟，只需要一台能 SSH 的 Linux 服务器（systemd 发行版：Debian、Ubuntu、CentOS、Rocky、Alpine 等）。

仓库里**不含任何运行数据**：没有账号、没有 API Key、没有密钥文件。
部署后你自己创建超级管理员、自己填上游账号，与任何其他实例无关。

- 运行时依赖：**无**。不需要 Python、Node.js、Java，也不需要 MySQL / Redis。
- 编译依赖：**Go 1.26+**（只在编译时需要，编完可以卸掉）
- 产物：一个二进制 + 一个 JSON 数据文件

---

## 一、最快路径：从 GitHub 一键部署

服务器上一条命令搞定：克隆源码、装 Go、编译、建用户、写配置、装 systemd、起服务、健康检查。

```bash
curl -fsSL https://raw.githubusercontent.com/LittIeRat/laskah/main/scripts/deploy-from-github.sh | sudo bash
```

不放心直接管道给 root 的话（应该不放心），先下载读一遍再执行：

```bash
curl -fsSLO https://raw.githubusercontent.com/LittIeRat/laskah/main/scripts/deploy-from-github.sh
less deploy-from-github.sh
sudo bash deploy-from-github.sh
```

可用环境变量：

| 变量 | 默认 | 用途 |
| --- | --- | --- |
| `LASKAH_REPO` | 本仓库 | 换成自己的 fork 或镜像地址 |
| `LASKAH_BRANCH` | `main` | 部署指定分支 |
| `LASKAH_SRC` | `/opt/laskah/src` | 源码检出位置 |

```bash
# 例：部署自己 fork 的 dev 分支
curl -fsSL https://raw.githubusercontent.com/LittIeRat/laskah/main/scripts/deploy-from-github.sh \
  | sudo LASKAH_REPO=https://github.com/你的用户名/laskah.git LASKAH_BRANCH=dev bash
```

脚本按顺序做：补齐 `git` / `curl` / `tar`（apt / dnf / yum / apk / pacman / zypper 自动识别）
→ 浅克隆或增量更新源码 → 调用 `scripts/install-linux.sh` 完成剩余安装。

**它同时是升级脚本**：再跑一次就是拉最新代码重编重启，
`/etc/laskah/laskah.env` 和 `/var/lib/laskah/db.json` 都会原样保留。

看到 `健康检查: {"ok":true,...}` 就算成功。

---

## 二、已有源码：本地安装脚本

手里已经有源码目录（比如别人发的压缩包）：

```bash
cd laskah
sudo LASKAH_AUTO_GO=1 bash scripts/install-linux.sh
```

`LASKAH_AUTO_GO=1` 表示缺 Go 或版本低于 `go.mod` 要求时，
自动从 [go.dev/dl](https://go.dev/dl/) 下载当前 stable 装到 `/usr/local/go`。
已经自己装好 Go 的话去掉这个变量即可。

> 发行版仓库里的 `golang-go` 常常低于要求版本，装了也编不过，建议交给脚本装官方包。

只想编译不想安装：

```bash
bash scripts/build.sh                                        # 当前架构
TARGETS="linux/amd64 linux/arm64" bash scripts/build.sh      # 交叉编译
```

验证服务：

```bash
curl -s http://127.0.0.1:8787/health
sudo systemctl status laskah --no-pager
```

---

## 三、暴露到公网（必须做）

服务只监听 `127.0.0.1:8787`，这是故意的——管理后台不应该裸奔在公网。
用 Caddy 最省事，证书自动签发续期：

```bash
sudo apt-get install -y caddy         # Debian/Ubuntu
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudo sed -i 's/laskah.example.com/你的域名/g' /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

用 Nginx 则是：

```bash
sudo cp deploy/nginx-laskah.conf /etc/nginx/sites-available/laskah
sudo ln -sf /etc/nginx/sites-available/laskah /etc/nginx/sites-enabled/laskah
sudo sed -i 's/laskah.example.com/你的域名/g' /etc/nginx/sites-available/laskah
sudo certbot --nginx -d 你的域名
sudo nginx -t && sudo systemctl reload nginx
```

两份配置默认把 `/admin/*` 限制在 `203.0.113.0/24` 网段。
**改成你自己的出口 IP 段**（`curl ifconfig.me` 查），或者删掉那几行 `allow/deny`
——删掉的话登录页会直接暴露在公网，靠密码和限速兜底。

代理配好后把域名填进 CORS 白名单：

```bash
sudo sed -i 's|^ALLOW_ORIGIN=.*|ALLOW_ORIGIN=https://你的域名|' /etc/laskah/laskah.env
sudo systemctl restart laskah
```

---

## 四、第一次打开：创建超级管理员

浏览器访问 `https://你的域名/`，会自动跳到 `/setup`。

填账户名（3–48 字符，区分大小写）和密码（至少 8 位），提交后**立刻存进密码管理器**。

存不存都由你自己负责，因为服务端记不住：账户名是 AES-256-GCM 密文落盘，
密码只存 PBKDF2-SHA256（240000 轮）散列，日志和终端横幅都刻意不打印凭据。
真忘了只能停服、删掉 `db.json` 里的 `config.users` 再重新 `/setup`。

初始化完成后 `/setup` 自动失效，之后只能走 `/login`。

**顺手备份 `MASTER_KEY`**（在 `/etc/laskah/laskah.env` 里）。
它是解密 `db.json` 的唯一钥匙，丢了所有已存的上游 Key 都成废数据。

---

## 五、加上游账号

在 `/manage` 里按顺序来：

1. **创建分组**，输个名字就行。分组可以随时启用 / 禁用，禁用后整组账号立刻退出分配池、数据保留。
2. **创建账号** → 居中弹窗里填：
   - 用户名称（只是界面上认人用的）
   - API Key 批量粘贴框，一行一个，**单账号上限 5 个**
   - Base URL，如 `https://api.newapi.com/v1`，请求时自动拼 `/chat/completions`
   - 点「获取模型列表」→ 勾选要开放的模型（留空 = 全部接受）
   - 额度查询（可选）：请求地址、访问令牌、用户 ID、超时秒数、自动查询间隔
3. **确定保存**。保存后这个账号**只能查余额或删号**，配置不可修改、也不会回显给任何人。

没配额度查询的账号按「∞ 无限余额」处理，不参与金额汇总，也不会被余额清理逻辑删掉。

余额耗尽会自动删号，三条触发路径：后台按间隔扫描、请求到达时发现数据过期先查一次、
上游明确报「这一次都付不起」时立刻删号换账号重试。第三条既认显式文案
（`余额不足` / `insufficient_quota` / `credit balance is too low` 等），
也认只报金额的预扣费失败，例如 `预扣费额度失败, 用户剩余额度: ＄0.18, 需要预扣费额度: ＄0.29`
——折叠全角字符后比较「剩余 < 需要」。限流和 5xx 不算余额耗尽，不会误删正常账号。

---

## 六、调用

在 `/dashboard` 创建网关密钥（支持批量），然后当成 OpenAI 用：

```bash
curl https://你的域名/v1/chat/completions \
  -H "Authorization: Bearer 你的网关密钥" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"你好"}]}'
```

Base URL 填 `https://你的域名/v1`，兼容 `/chat/completions`、`/completions`、
`/embeddings`、`/models`。`stream:true` 走 SSE。

`GET /v1/models` 只返回你在账号里勾选过的模型，每项固定 `id` / `object` / `created` / `owned_by`
四个字段，`owned_by` 统一署名 `laskah`，不泄露真实上游。

---

## 七、日常运维

```bash
sudo systemctl status laskah --no-pager   # 状态
sudo journalctl -u laskah -f              # 实时日志
sudo systemctl restart laskah             # 重启

# 备份（两个文件都要，缺一不可解密）
sudo cp /var/lib/laskah/db.json ~/laskah-db-$(date +%F).json
sudo cp /etc/laskah/laskah.env ~/laskah-env-$(date +%F).bak

# 升级：重跑远程脚本即可拉最新代码重编重启，env 与数据都保留
curl -fsSL https://raw.githubusercontent.com/LittIeRat/laskah/main/scripts/deploy-from-github.sh | sudo bash

# 或在已有源码目录里
sudo LASKAH_AUTO_GO=1 bash scripts/install-linux.sh
```

---

## 排障

| 现象 | 处理 |
| --- | --- |
| 脚本报找不到 `go` | 加 `LASKAH_AUTO_GO=1` 让脚本自动装，或自己装后 `export PATH=/usr/local/go/bin:$PATH` |
| `git clone` 卡住或超时 | 国内机器访问 GitHub 不稳。有代理就 `export https_proxy=http://127.0.0.1:7890` 后重跑；没代理换镜像 `LASKAH_REPO=https://ghproxy.net/https://github.com/LittIeRat/laskah.git` |
| 脚本报 `$'\r': command not found` | 脚本被 Windows 改成 CRLF 了，`sed -i 's/\r$//' 脚本名` 修掉；仓库里已用 `.gitattributes` 强制 LF |
| 启动失败 | `sudo journalctl -u laskah -n 50 --no-pager`；多半是 env 写错或 `DATA_FILE` 目录不在 `ReadWritePaths` 里 |
| 登录页 403 | 反代的 `/admin/*` IP 白名单没改成你的网段 |
| 登录报 429 | 触发限速（10 分钟 5 次失败锁 15 分钟），等一会儿或重启服务清计数 |
| 流式响应挤在一起才吐 | 反代没关缓冲，用 `deploy/` 里给的配置，别自己写 |
| 调用返回 503 | 分组被禁用、账号全被删光、或所有 Key 都在冷却，去 `/dashboard` 看 |

更细的内容（systemd 单元逐项说明、全部环境变量、角色权限、负载均衡策略、
安全设计）见同目录的 `DEPLOY.md`。
