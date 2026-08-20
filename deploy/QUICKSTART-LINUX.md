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
   - Base URL，如 `https://api.newapi.com/v1`，请求时自动拼 `/chat/completions` / `/responses` / `/models`
   - 自定义端点（可选）：上游路径不标准时，分别填 chat / responses / models 的**完整地址**（必须带 `http(s)://` 和域名）
   - 点「获取模型列表」→ 勾选要开放的模型（留空 = 全部接受）
   - 计价（可选）：不计价 / 按量（每 1M tokens 多少钱）/ 按次（一次多少钱）
   - 手动配置余额（可选）：开启后填初始余额，本站按自算 token 扣减，不依赖上游额度接口
   - 额度查询（可选）：请求地址、访问令牌、用户 ID、超时秒数（1–300，默认 30）、自动查询间隔
   - 脚本查询（可选）：额度查询完整地址 + 一段查询脚本，用来对接不是 New API 的站点；
     两处都留空表示这个站点没有额度接口，账号就按「∞ 无限余额」走
   - 频率限制（可选）：不开启 = 无限制；开启后填「一分钟能请求多少次」
3. **确定保存**。保存后这个账号**只能查余额、启停或删除**，配置不可修改、也不会回显给任何人。

既没配额度查询（内置凭据或查询脚本）、也没开手动余额的账号按「∞ 无限余额」处理，不参与金额汇总，也不会被余额清理逻辑暂停。
开了手动余额的账号不算无限：余额由「初始余额 − 本站自算消耗」得出，扣到下限同样自动暂停。

### 站点不是 New API 怎么查余额

填一段查询脚本就行，形态与 cc-switch 一致，创建账号弹窗的「脚本查询」区块里有两个示例可以直接插入：

```js
({
  request: {
    url: "{{baseUrl}}/api/usage",
    method: "POST",
    headers: { "Authorization": "Bearer {{apiKey}}" }
  },
  extractor: function (response) {
    return { isValid: !response.error, remaining: response.balance, unit: "USD" };
  }
})
```

- 可用变量：`{{baseUrl}}`、`{{apiKey}}`、`{{accessToken}}`、`{{userId}}`。
- 额度接口和推理地址不同源时，把完整地址填到「额度查询完整地址」，`{{baseUrl}}` 就取它。
- `extractor` 返回 `remaining`（或 `total` + `used`）即可；返回 `isValid: false` 会把这次查询判为失败并显示 `invalidMessage`。
- 保存前点「校验脚本」确认最终请求：只解析和替换变量，**不会真的发请求**，凭据也会遮蔽后回显。
- 脚本跑在纯 Go 写的沙箱里：没有网络、文件、时间和随机数，禁用 `require` / `eval` / `Function` / `new`，并有源码与执行步数上限。
- 脚本和访问令牌同级加密落盘，保存后不回显——里面写死的令牌不会再被任何人读出来。

**额度查询超时**默认 30 秒，是「单次请求」的上限而不是整轮共享：内置查询会依次尝试
`/api/status`（另有 8 秒上限，卡住只影响换算单位）、`/api/user/self`、`/api/usage`，
每一步各自计时。站点挂在 Cloudflare 后面时冷连接握手很容易超过 10 秒，
旧版默认 10 秒会频繁报 `context deadline exceeded`；
遇到仍然超时的站点，删号重建时把「超时时间」填到 60–300 秒。

额度查询**不会拖慢聊天请求**：请求路径上最多等 5 秒（`REQUEST_REFRESH_WAIT_MS`），
到点先放行，查询继续在后台跑完并写回结果供下一次请求判定；客户端断开也不会取消它。

**token 数与金额全部由本站自己算**，不采用上游返回的 `usage`——有些中转站会谎报 token。
上游自报的数字仍会记在 `upstreamTokens` 里供对照，返回给下游的 `usage` 则是本站口径。

**余额安全线 $0.50**：实际生效的下限是 `max(你填的最低余额, 0.50)`，
账号行上会显示当前生效的「余额下限」，快到线时提前挂一个「接近下限」提示。
余额只剩几毛钱时上游大概率连一次预扣费都过不了，所以余额 `<= 0.50` 就直接暂停账号，
而不是等它报错——否则调用方要先吃一次失败。

余额耗尽会**自动暂停账号（不删数据）**，触发路径：

1. 后台按「自动查询间隔」扫描到期账号；
2. 请求到达时发现余额数据过期，先查一次再放行；
3. 上游明确报「这一次都付不起」时立刻暂停并换账号重试。既认显式文案
   （`余额不足` / `insufficient_quota` / `credit balance is too low` 等），
   也认只报金额的预扣费失败，例如 `预扣费额度失败, 用户剩余额度: ＄0.18, 需要预扣费额度: ＄0.29`
   ——折叠全角字符后比较「剩余 < 需要」。限流和 5xx 不算余额耗尽，不会误停正常账号。
4. HTTP 200 但响应体 `error` 字段报余额不足，以及流式响应中途的 SSE `error` 事件。
   只看 `error` 字段，模型正文里出现「余额不足」字样不会误停账号。

被暂停的账号在 `/manage` 里带「余额不足已暂停」徽章并显示原因。
充值后把该行的开关打开即恢复：服务端会顺带刷一次余额，若仍低于下限会再次暂停并提示。

**账号级频率限制**：填了「每分钟请求次数」的账号用满配额后，本分钟内网关自动换用其它账号，
调用方不会看到 429；只有全部账号都超限时才返回 503。密钥上的 `rateLimitPerMin` 仍然是直接 429。

流式请求遇到余额不足：还没下发任何内容时直接换号重来（调用方无感）；
已经下发了一部分就立刻截断，补 `finish_reason: "length"` 和 `data: [DONE]` 正常收尾，
同时暂停该账号，下一次请求落到健康账号——不会让客户端干等或读到半截 SSE。

---

## 六、调用

在 `/dashboard` 创建网关密钥（支持批量），然后当成 OpenAI 用：

```bash
curl https://你的域名/v1/chat/completions \
  -H "Authorization: Bearer 你的网关密钥" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"你好"}]}'
```

Responses 兼容端点同样可用：

```bash
curl https://你的域名/v1/responses \
  -H "Authorization: Bearer 你的网关密钥" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","input":"你好"}'
```

Anthropic 客户端（Claude Code、Anthropic SDK）用 `/v1/messages` + `x-api-key`：

```bash
curl https://你的域名/v1/messages \
  -H "x-api-key: 你的网关密钥" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}'
```

Base URL 填 `https://你的域名/v1`，兼容 `/chat/completions`、`/responses`、`/messages`、
`/completions`、`/embeddings`、`/models`。`stream:true` 走 SSE，
`/v1/messages` 的流式返回是原生 Anthropic 事件序列。

上游忽略 `max_tokens` 时本站会自己收口（硬上限 = 声明值 × 1.25 + 8），
非流式裁正文并把 `finish_reason` 改成 `length`，流式在上限处正常收尾，计费只算真正下发的部分。

**不带密钥直接打开 `/v1/models`** 会列出全站可用账号提供的模型并集（公开目录），
浏览器里就能直接确认这个站支持什么模型，`/v1/models/{模型名}` 也可匿名查询；
带了错误密钥仍是 401 / 403。想对外隐藏支持范围就在 env 里设 `PUBLIC_MODELS=false`。

`/dashboard` 上的「查询总余额」会先刷新全部账号再给一份报告，把总额拆成
「上游查询得到」与「手动余额本地扣减」，并说明有几个账号查询失败（失败账号沿用旧余额）。
内置 New API 查询与自定义脚本查询都归入「上游查询得到」，报告里另有一行给出脚本账号数与脚本异常数。

> 用 `http://ip:port` 明文访问时，浏览器禁止网页写系统剪贴板，所以「复制」按钮会回落成
> 「弹出一个已全选的文本框，请按 Ctrl+C」。配好域名与 HTTPS 后即可一键复制。

`GET /v1/models` 只返回你在账号里勾选过的模型，每项固定 `id` / `object` / `created` / `owned_by`
四个字段，`owned_by` 统一署名 `laskah`，不泄露真实上游。

**请求某个模型时只会用有这个模型的账号**：网关先读 `model` 再挑账号，
所以请 `claude-3-opus` 不会落到只挂 gpt 系列 Key 的账号上；没有任何账号提供该模型时返回 503。
因此不同分组/账号可以按模型分工，客户端只需要一个 Base URL。

创建账号时**不勾任何模型等于「不限」**，这类账号什么模型都收——但「没设限制」不代表它真的有。
所以只要有账号明确勾选了这个模型，请求就只在这些账号里挑，兜底账号（没勾模型的）留到无人声明时才用，
哪怕它余额高得多。想按模型分工就老老实实在每个账号里勾清楚模型列表，效果最好。

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

### 忘记口令 / 重置后登不进去

二进制自带命令行自救入口，不用清库重装：

```bash
sudo systemctl stop laskah                       # 必须先停，否则运行中的进程会把旧数据盖回磁盘
sudo -u laskah /opt/laskah/laskah list-admins    # 列出账户（账户名脱敏，只用来确认目标）
sudo -u laskah /opt/laskah/laskah reset-password 'Digital Gleam' '新口令至少八位'
sudo systemctl start laskah
```

子命令要读到与服务相同的 `DATA_FILE` 与 `MASTER_KEY`。若这两个变量写在
`/etc/laskah/laskah.env`，就用下面这种带 env 的写法：

```bash
sudo systemctl stop laskah
sudo -u laskah env $(grep -E '^(DATA_FILE|MASTER_KEY)=' /etc/laskah/laskah.env | xargs) \
  /opt/laskah/laskah reset-password 'Digital Gleam' '新口令至少八位'
sudo systemctl start laskah
```

重置会顺带把该账户置为启用。口令的首尾空白一律被忽略，所以从密码管理器粘贴带上尾随
空格或换行不会影响登录；口令中间的空格有效。

---

## 排障

| 现象 | 处理 |
| --- | --- |
| 脚本报找不到 `go` | 加 `LASKAH_AUTO_GO=1` 让脚本自动装，或自己装后 `export PATH=/usr/local/go/bin:$PATH` |
| `git clone` 卡住或超时 | 国内机器访问 GitHub 不稳。有代理就 `export https_proxy=http://127.0.0.1:7890` 后重跑；没代理换镜像 `LASKAH_REPO=https://ghproxy.net/https://github.com/LittIeRat/laskah.git` |
| 脚本报 `$'\r': command not found` | 脚本被 Windows 改成 CRLF 了，`sed -i 's/\r$//' 脚本名` 修掉；仓库里已用 `.gitattributes` 强制 LF |
| 启动失败 | `sudo journalctl -u laskah -n 50 --no-pager`；多半是 env 写错或 `DATA_FILE` 目录不在 `ReadWritePaths` 里 |
| 登录页 403 | 反代的 `/admin/*` IP 白名单没改成你的网段 |
| 登录报 429 | 触发限速（10 分钟 5 次失败锁 15 分钟），响应里带剩余分钟数；等待或 `systemctl restart laskah` 清计数 |
| 改密/重置后新口令登不进去 | 先确认不是 429 锁定；再用 `laskah reset-password` 在服务器上重置（见上一节）。口令首尾空白会被忽略，别把空格当成口令的一部分 |
| 流式响应挤在一起才吐 | 反代没关缓冲，用 `deploy/` 里给的配置，别自己写 |
| 调用返回 503 | 分组被禁用、账号全被删光、或所有 Key 都在冷却，去 `/dashboard` 看 |

更细的内容（systemd 单元逐项说明、全部环境变量、角色权限、负载均衡策略、
安全设计）见同目录的 `DEPLOY.md`。
