# Laskah 服务器部署手册

Laskah 是单文件 Go 程序：没有运行时依赖，不需要 Python / Node.js / Java，也不需要数据库。
一个二进制 + 一个 JSON 数据文件 + 一个主密钥文件就是全部。

- 监听：默认 `127.0.0.1:8787`，公网流量经 Nginx / Caddy 反向代理进来
- 数据：`/var/lib/laskah/db.json`（上游 API Key、访问令牌、网关密钥均 AES-256-GCM 加密）
- 主密钥：`MASTER_KEY` 环境变量（推荐）或 `/var/lib/laskah/db.master.key`
- 健康检查：`GET /health`

> **只想快点跑起来**：服务器上一条命令即可，不必读本文。
>
> ```bash
> curl -fsSL https://raw.githubusercontent.com/LittIeRat/laskah/main/scripts/deploy-from-github.sh | sudo bash
> ```
>
> 细节见 [QUICKSTART-LINUX.md](QUICKSTART-LINUX.md)。
> 本文是逐步手册，解释每一步在做什么、为什么这么设，适合要自定义或排障时读。

---

## 1. 编译

### 在服务器上直接编译（推荐）

```bash
git clone --depth 1 https://github.com/LittIeRat/laskah.git
cd laskah
bash scripts/build.sh                                     # 当前架构
TARGETS="linux/amd64 linux/arm64" bash scripts/build.sh    # 交叉编译
```

### 在 Windows 开发机上交叉编译

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\build.ps1
```

产物在 `bin\`：

| 文件 | 用途 |
| --- | --- |
| `laskah.exe` | 本机预览 |
| `laskah-linux-amd64` | 绝大多数云主机（Intel / AMD） |
| `laskah-linux-arm64` | ARM 机型（AWS Graviton、阿里云 ARM、树莓派） |

脚本会打印每个产物的 SHA256，上传后请在服务器上比对。

手动编译等价命令：

```powershell
$env:CGO_ENABLED='0'; $env:GOOS='linux'; $env:GOARCH='amd64'
go build -trimpath -ldflags '-s -w' -o bin/laskah-linux-amd64 ./cmd/laskah
```

`-trimpath` 去掉源码绝对路径，`-s -w` 去掉符号表与调试信息：
既减小体积，也让二进制更难被反编译还原结构。
前端资源（HTML / CSS / JS / logo）通过 `go:embed` 编进二进制，服务器上不需要静态目录。

---

## 2. 上传

在服务器上直接 `git clone` 编译的话跳过本节。
从开发机推二进制上去时：

```powershell
scp bin\laskah-linux-amd64 root@YOUR_SERVER:/tmp/laskah
scp deploy\laskah.service  root@YOUR_SERVER:/tmp/
scp deploy\laskah.env.example root@YOUR_SERVER:/tmp/
```

服务器上校验：

```bash
sha256sum /tmp/laskah   # 与 build 脚本打印的哈希一致才继续
```

---

## 3. 系统准备

```bash
# 专用系统账户，不给登录 shell
sudo useradd --system --no-create-home --shell /usr/sbin/nologin laskah

# 程序目录（只读）与数据目录（可写）
sudo install -d -o root  -g root  -m 0755 /opt/laskah
sudo install -d -o laskah -g laskah -m 0700 /var/lib/laskah
sudo install -d -o root  -g root  -m 0750 /etc/laskah

# 安装二进制
sudo install -o root -g root -m 0755 /tmp/laskah /opt/laskah/laskah
```

数据目录必须是 `0700` 且属主为 `laskah`：`db.json` 里是加密后的凭据，
`db.master.key` 是解密它们的钥匙，两者都不能让其他本地用户读到。

---

## 4. 环境配置

```bash
sudo cp /tmp/laskah.env.example /etc/laskah/laskah.env
sudo chown root:root /etc/laskah/laskah.env
sudo chmod 600 /etc/laskah/laskah.env

# 生成主密钥与管理令牌，填进配置文件
openssl rand -base64 48   # -> MASTER_KEY
openssl rand -hex 24      # -> ADMIN_TOKEN

sudo nano /etc/laskah/laskah.env
```

必改项：

| 变量 | 说明 |
| --- | --- |
| `MASTER_KEY` | 数据加密主密钥。设置后主密钥不落盘，**丢失等于所有已存凭据不可解密** |
| `ADMIN_TOKEN` | 管理 API 的 Bearer 令牌，等价超级管理员权限，仅脚本调用需要 |
| `ALLOW_ORIGIN` | 收紧到自己的域名，别留 `*` |
| `TRUST_PROXY` | 反向代理后必须为 `true`，否则登录限速会把所有访客算作同一个 IP |

其余可调项（`HOST` / `PORT` / `DATA_FILE` / `STRATEGY` / `MAX_RETRIES` /
`COOLDOWN_MS` / `FAILURE_THRESHOLD` / `BALANCE_INTERVAL_MS` /
`REQUEST_REFRESH_WAIT_MS` / `PUBLIC_MODELS`）见
`deploy/laskah.env.example` 内注释。

`REQUEST_REFRESH_WAIT_MS` 默认 `5000`：请求命中账号且余额过期时，最多等这么久等余额查完，
到点先放行，查询继续在后台跑完并写回。想让额度接口查得更久，改的是账号自己的
「超时时间」（1–300 秒，默认 30），不是这个值。

`PUBLIC_MODELS` 默认 `true`：不带密钥访问 `/v1/models` 会列出全部可用账号提供的
模型并集（只有模型名，`owned_by` 统一署名 `laskah`）。需要对外隐藏供货范围时设为
`false`，匿名请求退回空列表，匿名单模型查询一律 404。

**超级管理员账号不在配置文件里。** 服务首次启动处于「待初始化」状态，
只有 `/setup` 可用，账号密码由你在浏览器里亲手创建（见第 7 节）。
仅在无人值守批量部署时才使用 `ADMIN_USER` / `ADMIN_PASSWORD` 跳过引导，
用完请立刻从 env 文件里删除并重启。

---

## 5. systemd 托管

```bash
sudo cp /tmp/laskah.service /etc/systemd/system/laskah.service
sudo systemctl daemon-reload
sudo systemctl enable --now laskah
sudo systemctl status laskah --no-pager
```

单元文件已经收紧权限：`NoNewPrivileges`、`ProtectSystem=strict`、
`CapabilityBoundingSet=`（清空）、`MemoryDenyWriteExecute`，只有
`ReadWritePaths=/var/lib/laskah` 可写。如果改了 `DATA_FILE` 位置，
必须同步改 `ReadWritePaths`，否则启动会因为无法写盘而失败。

验证：

```bash
curl -s http://127.0.0.1:8787/health
# {"ok":true,"groups":0,"accounts":0,"providers":0,"keys":0}
```

日志：

```bash
sudo journalctl -u laskah -f
```

---

## 6. 反向代理与 TLS

两份现成配置任选其一：

- `deploy/nginx-laskah.conf` → `/etc/nginx/sites-available/laskah`
- `deploy/Caddyfile` → `/etc/caddy/Caddyfile`（自动签发续期证书，省一步 certbot）

### Nginx

```bash
sudo cp deploy/nginx-laskah.conf /etc/nginx/sites-available/laskah
sudo ln -sf /etc/nginx/sites-available/laskah /etc/nginx/sites-enabled/laskah
sudo sed -i 's/laskah.example.com/your-domain.com/g' /etc/nginx/sites-available/laskah
sudo certbot --nginx -d your-domain.com
sudo nginx -t && sudo systemctl reload nginx
```

### Caddy

```bash
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudo sed -i 's/laskah.example.com/your-domain.com/g' /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

两份配置都做了三件关键事：

1. **管理面限网段**：`/admin/*` 只允许可信 IP 段。把 `203.0.113.0/24`
   换成你的办公网或 VPN 网段；不想限制就删掉 `allow/deny` 段，但登录页会直接暴露在公网。
2. **网关端点关缓冲**：`stream:true` 走 SSE，开缓冲会把 token 攒住直到响应结束。
3. **读超时放宽到 600s**：长上下文推理比默认 60s 慢得多。

---

## 7. 首次访问：创建超级管理员

浏览器打开 `https://your-domain.com/`，会自动跳到 `/setup`。

1. 填写超级管理员账户名（3–48 字符，区分大小写）与密码（至少 8 位）
2. 提交后**立刻把账号与密码存进密码管理器**

为什么必须现在存：

- 账户名以 AES-256-GCM 密文 + SHA-256 摘要落盘，服务端**无法反查出明文**
- 密码只存 PBKDF2-SHA256（240000 轮）散列，同样不可逆
- 终端横幅与日志刻意不打印任何账户名或口令

口令至少 8 个字符，**首尾空白一律忽略**：创建、改密、重置、登录共用同一个归一化规则，
所以从密码管理器粘贴带上的尾随空格或换行不会导致改密后登不进去；中间的空格是有效字符。

初始化完成后 `/setup` 自动失效（重复提交返回 409），只能从 `/login` 进入。

### 忘记口令的恢复流程

不需要清库重装，用二进制自带的子命令重置（只能在服务器本机执行，需要数据文件与主密钥）：

```bash
sudo systemctl stop laskah                       # 必须先停：运行中的进程会把内存态盖回磁盘
sudo -u laskah /opt/laskah/laskah list-admins    # 列出账户（账户名脱敏）
sudo -u laskah /opt/laskah/laskah reset-password 'Digital Gleam' '新口令至少八位'
sudo systemctl start laskah
```

若 `DATA_FILE` / `MASTER_KEY` 写在 `/etc/laskah/laskah.env`，执行时要把它们带上：

```bash
sudo -u laskah env $(grep -E '^(DATA_FILE|MASTER_KEY)=' /etc/laskah/laskah.env | xargs) \
  /opt/laskah/laskah reset-password 'Digital Gleam' '新口令至少八位'
```

重置会顺带把该账户置为启用状态，避免「口令对了但账户被禁用」的二次卡死。
只有连一个超管账户都不剩时才需要走「删除 `db.json` 里的 `config.users` → 重新 `/setup`」。

### 角色分级

| 角色 | 可访问 |
| --- | --- |
| 超级管理员 | 全部页面：`/dashboard`、`/manage`，以及全部 `/admin/*` 接口 |
| 管理员 | 只有 `/dashboard`。手输 `/manage` 会被服务端 302 回看板，直接调管理接口返回 403 |

在 `/manage` 的「管理员账户」区块添加、停用、改密或删除管理员。
权限判定完全在服务端会话里，前端不参与，改地址栏或伪造请求都不生效。

---

## 8. 配置上游账号

在 `/manage` 按顺序操作：

1. **创建用户分组**：输入名称即可。分组可随时启用 / 禁用，禁用后该组账号立即退出分配池，数据保留。
2. **创建账号**：点「创建账号」后在居中弹窗里填
   - 用户名称（仅用于界面识别）
   - API Key 批量粘贴框：每行一个，**单账号上限 5 个**
   - Base URL：如 `https://api.newapi.com/v1`，请求时自动拼 `/chat/completions` / `/responses` / `/models`
   - 自定义端点（可选）：chat / responses / models 各自的**完整地址**，用于上游路径不标准的站点；
     必须是带域名的 `http(s)://` 绝对地址，只填其中一个也可以，其余仍按 Base URL 拼
   - 「获取模型列表」→ 勾选要开放的模型（留空表示接受全部）
   - 计价方式（可选）：`不计价` / `按量`（每 1M tokens 单价）/ `按次`（每次请求单价）
   - 手动配置余额（可选）：开启后填初始余额（USD），余额 = 初始余额 − 本站自算消耗
   - 额度查询（可选）：请求地址、访问令牌、用户 ID、超时秒数、自动查询间隔（分钟，0 = 不自动查）
   - 脚本查询（可选）：额度查询完整地址 + 一段 `({ request, extractor })` 查询脚本，用于对接任意非 New API 站点；
     填了脚本就不再走内置查询路径，点「校验脚本」可在保存前确认最终请求（只解析、不发请求）。
     两处都留空表示该站点没有额度接口，账号按「∞ 无限余额」处理
   - 频率限制（可选）：不开启 = 无限制；开启后填「每分钟请求次数」，达到上限时网关自动换号
3. **确定保存配置**。保存后该账号**只能查余额、启停或删除**，配置不可修改也不会回显。

既未配置额度查询（内置凭据或查询脚本）、也未开启手动余额的账号按「∞ 无限余额」处理，既不会被余额清理逻辑暂停，
也不会去打上游额度接口。开了手动余额的账号则始终按本地数字判定，点「刷新」返回本地余额视图。

### token 计量与计费口径

**上游返回的 `usage` 不参与任何计费与额度判定**，只作为对照值存进 `upstreamTokens`：
实测存在中转站放大 token 数的情况。本站自己数输入与输出 token（流式按分片累计，
半个 UTF-8 字符留到下一片），并把响应体里的 `usage` 改写成本站口径后再返回给下游。

金额按账号计价方式结算：`per_mtoken` 为 `单价 × 总 token / 1e6`，`per_call` 为 `单价 × 次数`。
手动余额账号每次结算后直接扣本地余额，扣到下限即暂停；下限取
`max(自填最低余额, 一次请求的预估花费)`，不套用上游查询模式那条 $0.50 固定安全线。

`/dashboard` 的「查询总余额」会先刷新全部账号，再把总额拆成「上游查询得到」与
「手动余额本地扣减」两部分，并报告本次查询失败 / 暂停 / 删除的账号数——
有账号查询失败时总额沿用旧值，报告会明确提示可能偏高。

### 自动暂停的三条触发路径

余额耗尽**不删账号**，而是把它暂停：退出分配池，但账号、名下 API、余额与统计全部保留。
充值后在 `/manage` 账号行打开开关即恢复（服务端会顺带刷一次余额）。

1. 后台按各账号自身的「自动查询间隔」到期扫描，余额低于阈值即暂停
2. 请求到达时若余额数据超过 `requestRefreshSec` 秒未更新，先查一次再分配
3. 上游明确报「这一次请求都付不起」时立刻暂停并换账号重试，调用方无感知

第 3 条同时覆盖显式文案（余额不足 / insufficient_quota / credit balance is too low ...）
和只报金额的预扣费失败，例如：

```
预扣费额度失败, 用户剩余额度: ＄0.182898, 需要预扣费额度: ＄0.290486
```

判定会折叠全角字符后比较「剩余 < 需要」。限流（rate limit）与 5xx 一律不算余额耗尽，
避免上游抖动误停正常账号。

### 账号级频率限制

账号上填了「每分钟请求次数」后，该账号在窗口内用满配额就本分钟不再参与分配，
网关直接换用其它账号（不是给调用方 429）。全部账号都超限时才返回 503。
超限只影响这一次落点，`key.accountId` 的常驻绑定不变，窗口滑过后请求自动回到原账号。

---

## 9. 下游调用

在 `/dashboard` 创建网关密钥（支持批量），然后：

```bash
curl https://your-domain.com/v1/chat/completions \
  -H "Authorization: Bearer <网关密钥>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"你好"}]}'
```

| 端点 | 说明 |
| --- | --- |
| `POST /v1/chat/completions` | 主入口，支持 `stream:true` |
| `POST /v1/responses` | OpenAI Responses 兼容，支持 `stream:true` |
| `POST /v1/messages` | Anthropic Messages 兼容（Claude Code / Anthropic SDK），支持 `stream:true` 与 `x-api-key` |
| `POST /v1/completions` | 旧版补全，内部转成 messages |
| `POST /v1/embeddings` | 向量化 |
| `GET /v1/models` | 严格 OpenAI 规范：`{"object":"list","data":[{"id","object","created","owned_by"}]}`；不带密钥时返回公开模型目录（可用 `PUBLIC_MODELS=false` 关闭） |
| `GET /v1/models/{id}` | 单模型查询，匿名同样可用 |

`/v1/responses` 与 `/v1/chat/completions` 共用同一套账号分配、余额判定、频率限制、
截断换号与本地计量逻辑，只是请求 / 响应结构不同：

```bash
curl https://your-domain.com/v1/responses \
  -H "Authorization: Bearer <网关密钥>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","input":"你好"}'
```

Anthropic 客户端把 Base URL 填 `https://your-domain.com`，鉴权用 `x-api-key`：

```bash
curl https://your-domain.com/v1/messages \
  -H "x-api-key: <网关密钥>" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}'
```

Base URL 填 `https://your-domain.com/v1`。不带 `/v1` 前缀的同名路径也受支持，
方便某些只认 `/chat/completions` 的客户端。

**思维链计量**：推理模型放在 `reasoning_content` / `reasoning` 的内容计入本地输出 token，
转 Anthropic 时单独走 `thinking` 块，不与正文混在同一个内容块里。

**输出上限兜底**：上游忽略 `max_tokens` 时本站按「声明值 × 1.25 + 8」自己收口，
非流式裁正文并把 `finish_reason` 改成 `length`，流式在上限处正常收尾。计费只算真正下发的部分。

`/v1/models` 只列出该密钥真正能调到的模型：账号可用、上游启用、密钥白名单允许，
通配符（`gpt-4*`）只用于请求期匹配，不会作为可枚举 id 暴露。`owned_by` 统一为
`laskah`，不泄露真实上游站点。

---

## 10. 备份与恢复

要备份的只有两个文件：

```bash
sudo systemctl stop laskah
sudo tar czf laskah-backup-$(date +%F).tar.gz -C /var/lib laskah
sudo systemctl start laskah
```

- `db.json` — 分组、账号、上游 API（密文）、网关密钥（密文）、用量统计
- `db.master.key` — 仅在未设置 `MASTER_KEY` 时存在

用了 `MASTER_KEY` 环境变量的部署，请把该密钥单独存进密码管理器：
**只有 `db.json` 没有主密钥，所有凭据都解不开。**

恢复：停服 → 覆盖 `/var/lib/laskah/` → 确认属主 `laskah:laskah` 与权限 `0700` → 启动。

热备份（不停服）也可行——写盘是原子替换——但快照可能落在两次统计刷盘之间，
丢失最多 30 秒的用量计数。

---

## 11. 升级与回滚

用一键脚本部署的（源码在 `/opt/laskah/src`），升级就是重跑同一条命令：

```bash
sudo cp /opt/laskah/laskah /opt/laskah/laskah.prev   # 先留一份好回滚
curl -fsSL https://raw.githubusercontent.com/LittIeRat/laskah/main/scripts/deploy-from-github.sh | sudo bash
```

它会拉最新代码、重新编译、重启并做健康检查，`laskah.env` 与 `db.json` 都不动。

手工换二进制：

```bash
# 升级
sudo cp /opt/laskah/laskah /opt/laskah/laskah.prev
sudo install -o root -g root -m 0755 /tmp/laskah-new /opt/laskah/laskah
sudo systemctl restart laskah
curl -s http://127.0.0.1:8787/health

# 回滚
sudo cp /opt/laskah/laskah.prev /opt/laskah/laskah
sudo systemctl restart laskah
```

数据文件带 `version` 字段，启动时自动迁移到当前版本。**跨版本升级前先备份**：
迁移是单向的，旧版本读不了新版数据。

重启会清空全部登录会话（会话只在内存里），管理员需要重新登录；
网关密钥不受影响，下游调用不中断。

---

## 12. 安全清单

上线前逐条确认：

- [ ] `MASTER_KEY` 已设置且已单独备份，`ADMIN_TOKEN` 已改成随机值
- [ ] `/etc/laskah/laskah.env` 权限 `600`、属主 `root:root`
- [ ] `/var/lib/laskah` 权限 `0700`、属主 `laskah:laskah`
- [ ] 反向代理已启用 HTTPS，`/admin/*` 已限可信网段
- [ ] `TRUST_PROXY=true`，且代理确实注入 `X-Forwarded-For`
- [ ] `ALLOW_ORIGIN` 收紧到自己的域名
- [ ] 超级管理员凭据已存入密码管理器
- [ ] env 文件里没有残留 `ADMIN_USER` / `ADMIN_PASSWORD`
- [ ] 防火墙只放 80/443，`8787` 不对外
- [ ] 已配置 `db.json` 定期备份

服务端已内置的防护，不需要额外配置：

| 面向 | 措施 |
| --- | --- |
| 凭据存储 | 上游 API Key / 访问令牌 / 网关密钥 / 额度查询脚本全部 AES-256-GCM 加密落盘；口令 PBKDF2-SHA256 240000 轮 |
| 查询脚本沙箱 | 纯 Go 实现的 JS 子集解释器：无网络、无文件、无时间与随机数，禁 `require`/`eval`/`Function`/`new`；源码 16 KB、执行步数 200000 上限；HTTP 请求由宿主发出 |
| 账户名 | 超级管理员账户名密文 + 摘要存储，不可反查 |
| 登录爆破 | 按来源 IP 限速：10 分钟内 5 次失败锁 15 分钟；不区分「账号不存在」与「口令错误」，防账户枚举 |
| 会话 | 令牌只存摘要，HttpOnly + Secure + SameSite=Strict，8 小时绝对过期 / 90 分钟空闲过期 |
| CSRF | Cookie 会话的写请求必须携带匹配的 `X-CSRF-Token` |
| 越权 | 角色判定只读服务端会话；管理接口在中间件层 403，页面路由层 302 |
| 前端注入 | CSP 无 `unsafe-inline`，DOM 全部走 `textContent` 构建，无 HTML 字符串拼接 |
| 信息泄露 | `/health` 只报聚合数量；列表接口只回掩码；`owned_by` 不暴露上游；`Referrer-Policy: no-referrer` |
| 逆向 | `-trimpath -s -w` 去符号与路径；前端资源随二进制内嵌 |
| 资源 | 请求体上限 16MB；额度查询连接池限单主机并发；systemd `MemoryMax=512M` / `TasksMax=512` |

---

## 13. 排障

| 现象 | 排查方向 |
| --- | --- |
| 启动即退出 | `journalctl -u laskah -n 50`。常见是 `DATA_FILE` 不在 `ReadWritePaths` 里 |
| 访问总是跳 `/setup` | 还没创建超级管理员，或数据文件被换成了空库 |
| 登录报 429 | 触发限速（10 分钟 5 次失败锁 15 分钟），响应里带剩余分钟数；等待或重启服务清空计数 |
| 改密或重置后新口令登不进去 | 先排除 429 锁定（连试多次会被锁），再用 `laskah reset-password` 重置。口令首尾空白被忽略，不要把空格算进口令 |
| 流式响应一次性吐出 | 代理没关缓冲。Nginx 查 `proxy_buffering off`，Caddy 查 `flush_interval -1` |
| 上游一直 502 | `/manage` 看账号是否被自动暂停；检查 Base URL 是否多写了 `/chat/completions` |
| 余额显示「∞ 无限」 | 该账号既没配内置额度查询凭据、也没配查询脚本，属预期行为 |
| 脚本账号余额不更新 | 看板 `accounts.scriptBroken` 不为 0 表示脚本编译失败（多为数据迁移后语法不被支持），删号重建并用「校验脚本」确认 |
| 账号不接流量了 | 看 `/manage` 账号行的「已暂停」徽章与暂停原因（含上游原文），充值后打开开关即恢复 |
| 登录限速把所有人一起锁 | `TRUST_PROXY` 没开，全部请求被当成同一个来源 IP |
| 余额查询报 `context deadline exceeded` | 老版本的 10 秒默认超时太短。新版默认 30 秒（历史数据自动抬到 30），仍超时就删号重建、把「超时时间」调到 60–300 秒；查询站点在 Cloudflare 后面时冷连接握手常年超过 10 秒 |
| 手动刷新全部余额在浏览器里报网关超时 | 反向代理的 `/admin/` 读超时太短。Nginx 用 `deploy/nginx-laskah.conf` 里的 `proxy_read_timeout 240s`，Caddy 用 `transport http { read_timeout 240s }`。后台查询不受影响，刷新页面即可看到结果 |
| 聊天请求偶尔比上游慢几秒 | 请求路径上的余额刷新在等额度接口。`REQUEST_REFRESH_WAIT_MS` 调小（默认 5000），或把账号的「请求时刷新间隔」调大 |

---

## 14. 本地预览

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\build.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\start-local.ps1
# http://127.0.0.1:8787  → 首次进 /setup

# 接口级冒烟（会重置本地数据并留下一套演示数据）
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\smoke-local.ps1

powershell -NoProfile -File scripts\stop-local.ps1
```

`scripts\smoke-local.ps1` 覆盖 69 项断言：初始化、登录限速、角色隔离、CSRF、
分组启停、5 个 API 上限、无限余额、批量密钥、看板汇总、`/v1/models` 规范、
额度查询脚本校验与脚本账号创建、安全响应头、旧地址 `/keys → /dashboard` 重定向。

需要页面截图时：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\start-chrome.ps1
go run .\tools\devshot -user "<超管账户>" -password "<口令>" -pages "/dashboard,/manage" -theme dark -out _preview
```

`tools/devshot` 只是开发期工具（自带极简 CDP over WebSocket 客户端），不参与服务端运行。
