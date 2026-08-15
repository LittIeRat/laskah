# Laskah — API 负载均衡网关

Go 单二进制实现的 OpenAI 兼容负载均衡网关：把多个 New API 站点账号（每账号最多 **5** 个上游 API Key）聚合成一个入口，自动给调用方分配账号、在账号内轮转 Key，按需查询余额，余额耗尽自动删号。

不依赖 Python / Node.js / Java，也不需要数据库。前端由 `go:embed` 打进二进制，运行时只要一个可执行文件加一个 JSON 数据文件。

开源许可 MIT，见 [LICENSE](LICENSE)。

## 结构

```
分组 group ──▶ 账号 account ──▶ 上游 API provider ──▶ New API 站点
                  ▲                    ▲
            /manage 创建          每账号最多 5 个

网关密钥 key ──分配──▶ 账号 ──轮转──▶ 上游 API
   /dashboard 创建
```

- **分组 group**：`/manage` 里输入名称创建，账号与网关密钥都归属到分组。可随时启用 / 禁用，禁用后整组账号立即退出分配池、数据保留。
- **账号 account**：一个 New API 站点身份，持有余额查询凭据，名下 1–5 个上游 API Key。保存后凭据不可回显、不可修改，只能查余额或删号。
- **上游 API provider**：真正携带 `Authorization` 打上游的条目，批量粘贴导入。
- **网关密钥 key**：下游拿到的 `sk-...`，请求进来后粘性绑定一个可用账号，再在账号内按策略挑 Key。

余额耗尽或上游明确报「这一次都付不起」时，账号连同名下 API 一起删除，绑定过它的密钥自动解绑并在下次请求重新分配——调用方无感知。

## 部署

Linux 服务器一条命令（克隆源码、装 Go、编译、建用户、写配置、装 systemd、起服务、健康检查）：

```bash
curl -fsSL https://raw.githubusercontent.com/LittIeRat/laskah/main/scripts/deploy-from-github.sh | sudo bash
```

重跑同一条命令就是升级：拉最新代码重编重启，配置与数据原样保留。
手里已有源码目录时用 `sudo LASKAH_AUTO_GO=1 bash scripts/install-linux.sh`。

| 文档 | 内容 |
| --- | --- |
| [deploy/QUICKSTART-LINUX.md](deploy/QUICKSTART-LINUX.md) | 10 分钟上手：一键部署、反代与 HTTPS、建超管、加账号、调用、排障 |
| [deploy/DEPLOY.md](deploy/DEPLOY.md) | 逐步手册：systemd 单元逐项说明、全部环境变量、备份恢复、安全清单 |

## 本地预览（Windows）

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\build.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\start-local.ps1
# 停止
powershell -NoProfile -File scripts\stop-local.ps1
```

| 页面 | 地址 | 用途 |
| --- | --- | --- |
| 初始化 | http://127.0.0.1:8787/setup | **首次部署**创建超级管理员，之后自动失效 |
| 登录 | http://127.0.0.1:8787/login | 账户密码登录 |
| 数据看板 | http://127.0.0.1:8787/dashboard | 分组余额、消耗 Token 与金额、创建网关密钥 |
| 分组与账号 | http://127.0.0.1:8787/manage | 分组、账号、上游 API、管理员账户（仅超管） |

**没有默认管理员账号。** 服务首次启动处于待初始化状态，只有 `/setup` 可用，
账号密码由你在浏览器里亲手创建并自行保存：账户名 AES-256-GCM 加密落盘 + SHA-256 摘要索引，
口令只存 PBKDF2-SHA256（240000 轮）散列，服务端无法反查、终端与日志也不打印。
无人值守部署可用 `ADMIN_USER` / `ADMIN_PASSWORD` 跳过引导（安全性更低，用后即删）。

旧地址 `/keys` 301 重定向到 `/dashboard`。

## 角色分级

| 角色 | 可访问 |
| --- | --- |
| 超级管理员 super | 全部页面与全部 `/admin/*` 接口 |
| 管理员 admin | 只有 `/dashboard`；手输 `/manage` 被服务端 302 回看板，直接调管理接口 403 |

超管在 `/manage` 的「管理员账户」区块添加、停用、改密、删除管理员（上限 64）。
权限判定完全依据服务端会话中的角色，前端不参与；看板响应也按角色裁剪——
普通管理员拿不到网关密钥列表，而不是靠前端隐藏。停用或删除账户会立刻注销其全部会话。

## 主题

右上角三段控件切换 **深色 / 浅色 / 跟随系统**，选择存在 `localStorage("laskah.theme")`，
通过 `<html data-theme>` 生效。`auto` 时交给 `prefers-color-scheme` 媒体查询接管。
登录页与初始化页也带同样的切换器。

## 使用网关

本站 base url 后缀是 `/v1`，走 chat completions 兼容：

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer sk-你的网关密钥" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

支持 `stream: true`（SSE 原样透传）。端点：

- `POST /v1/chat/completions`（`/chat/completions` 亦可）
- `POST /v1/completions`（内部转成 chat 调用）
- `POST /v1/embeddings`
- `GET /v1/models`、`GET /v1/models/{model}`
- `GET /health`

上游类型支持 OpenAI 兼容与 Anthropic（`type: anthropic` 时自动做请求 / 响应 / 流式格式转换）。

## /v1/models 输出格式

严格遵循 OpenAI 规范，需要携带网关密钥（匿名 401）。列表：

```json
{
  "object": "list",
  "data": [
    { "id": "gpt-4o", "object": "model", "created": 1735689600, "owned_by": "laskah" },
    { "id": "gpt-4o-mini", "object": "model", "created": 1735689600, "owned_by": "laskah" }
  ]
}
```

`GET /v1/models/gpt-4o` 返回裸模型对象（不含 `object: list` 包装），未知模型 404，非 GET/HEAD 返回 405。

规范细节：

- 每项**只有** `id` / `object` / `created` / `owned_by` 四个字段，无任何额外键。
- `object` 恒为 `model`，列表的 `object` 恒为 `list`。
- `owned_by` 统一署名 `laskah`，不泄露真实上游站点与账号数量。
- `created` 是上游条目最早创建时间的 Unix 秒，恒大于 0。
- `data` 按 `id` 字典序升序。
- 只列出该密钥**真正调得通**的模型：启用中的 provider ∩ 密钥的 provider 白名单 ∩ 密钥的模型白名单 ∩ 当前绑定账号（未绑定时取分组内可用账号并集）。
- 通配符条目（如 `gpt-4*`）只用于请求期匹配，不出现在列表里；模型名允许含斜杠（`meta/llama-3`）。

## 在 /manage 创建分组与账号

先输入名称创建分组，再点「创建账号」——配置在**居中弹窗**里填写，确认后一次性保存：

| 字段 | 说明 |
| --- | --- |
| 所属分组 | 必选，账号归属的用户分组 |
| 用户名称 | 仅用于界面识别 |
| API Key 批量粘贴 | 大文本框，每行一个，**上限 5 个** |
| Base URL | 上游地址，请求时自动拼 `/chat/completions` |
| 获取模型列表 | 拉取上游 `/models` 后勾选要启用的模型，留空表示接受全部 |
| 请求地址 | 额度查询站点，留空复用 Base URL（默认 `https://api.newapi.com`） |
| 访问令牌 | New API「个人设置 → 安全设置」生成的 access_token |
| 用户 ID | New API 里的数字用户 ID，例如 `114514` |
| 超时时间 | 额度查询超时秒数，1–120，默认 10 |
| 自动查询间隔 | 分钟，0 表示不自动查询，0–1440 |
| 请求时刷新间隔 | 秒，0 表示关闭，0–3600，默认 60 |
| 余额下限 | 低于此值视为耗尽，默认 0；内置 **$0.50 安全线**，填 0 也按 $0.50 执行 |
| 余额耗尽自动删号 | 默认开启 |

批量粘贴支持每行一个 Key，同行内可用空白 / 逗号 / 分号分隔多个，`#` 与 `//` 开头为注释，
自动去重。超过 5 条的部分出现在响应 `skipped` 里并在界面提示，不会静默丢弃。

**未配置额度查询的账号显示「∞ 无限余额」**：既不参与金额汇总（避免 0 余额把总额拉低），
也不会被余额清理逻辑删掉，刷新时直接返回无限结果、不打上游。

**余额安全线 $0.50**（`store.MinBalanceFloorUSD`）：实际生效的下限是
`max(账号自填最低余额, 0.50)`，账号视图里的 `balanceFloor` 就是这个值。
余额只剩几毛钱时上游大概率连一次预扣费都过不了，等它报错再删号意味着调用方先吃一次失败，
所以余额 `<= balanceFloor` 直接判定耗尽并删号。无限额度账号不受此约束。

**保存后账号配置不可读、不可改**：接口只返回 `hasAccessToken` 之类的布尔标记，
`PATCH /admin/accounts/{id}` 直接 405。要改配置只能删号重建。

## 在 /dashboard 看数据与建密钥

- 顶部卡片：**账号总余额**（全部无限时显示 `∞ 无限`）、**累计消耗金额**（含已删号历史）、**消耗 Tokens 总数**、网关请求数、上游 API 数、网关密钥数。
- 分组卡片：每个分组各自的余额总量、消耗金额、消耗 Tokens、可用账号比例、启停状态。
- 单个创建密钥：名称、前缀、限定分组、模型白名单、Token 额度、每分钟限流、过期时间。
- 批量创建：数量 1–500 加名称前缀，生成 `前缀-01 … 前缀-NN`，每个密钥独立随机串。
- 支持导出 CSV、查看完整密钥、重置用量、批量删除。

密钥明文只在创建时和显式「查看明文」时返回，列表接口只给掩码。

## 余额查询与刷新

查询按顺序尝试（对应 New API 的两种脚本）：

1. `GET {请求地址}/api/status` 读 `quota_per_unit`（实测 `500000`，即 500000 quota = 1 USD）。
2. `GET {请求地址}/api/user/self`，带 `Authorization: Bearer {accessToken}`、`New-Api-User: {userId}`，用 `quota / quota_per_unit` 得余额，`used_quota / quota_per_unit` 得已消耗，`group` 作为套餐名。
3. 访问令牌不可用时回落 `POST {请求地址}/api/usage`（带 `Authorization: Bearer {上游 API Key}`），用 `balance` 作为余额。

三种刷新时机：

| 时机 | 触发方式 | 节流 |
| --- | --- | --- |
| 手动 | `/manage` 账号行「查看余额」、分组行「刷新余额」、「手动刷新全部余额」；`/dashboard` 顶部「手动刷新全部余额」 | 无，点即查 |
| 请求时 | 请求命中账号且余额数据过期时先查再放行 | 每账号 `requestRefreshSec` 秒一次，同账号并发合并成一次上游查询 |
| 后台 | 每分钟扫描到期账号 | 每账号 `queryIntervalMin` 分钟；为 0 的账号不自动查 |

**请求时刷新**是防止「单账号余额用完还不切号」的关键：`Authenticate` 挑到账号后若数据过期就先刷新，
刷新后判定不可用（余额耗尽或已被自动删除）就重新挑号，最多试 3 个账号，全不可用返回 503。
查询失败视为「仍可用」——网络抖动不该把正常账号踢出池子。

## 余额不足自动删号

上游报错时的判定（`gateway.IsBalanceExhausted`）：

- 状态码先粗筛，只有 `400/401/402/403/429` 才可能是余额问题；**5xx 与网络错误一律不判**，避免上游抖动误删账号。
- 文案折叠全角字符并转小写后匹配关键词：`余额不足` / `额度已耗尽` / `预扣费额度失败` / `欠费` / `insufficient_quota` / `credit balance is too low` 等。
- 只报金额、不含「不足」字样的情形也能识别——提取「剩余 X」与「需要 Y」两个数字，仅当 `X < Y` 时判定耗尽：

  ```
  预扣费额度失败, 用户剩余额度: ＄0.182898, 需要预扣费额度: ＄0.290486
  ```

  反之「剩余 ＄12.50，需要 ＄0.29」是正常提示，不会删号。
- 刻意不含 `rate limit` / `too many requests` 这类限流文案。

除状态码错误外，还有两条同样会触发删号的路径（都只看响应的 `error` 字段，
避免把模型正文里出现的「余额不足」字样误判成账号欠费）：

- **HTTP 200 但 body 带 `error`**：部分上游用 200 包着错误返回，状态码粗筛拦不住。
- **流式响应中途的 SSE `error` 事件**：流的 HTTP 状态在第一个字节前就是 200，
  余额不足只能从事件里读到。

命中后立刻删号并重新鉴权换账号重试（上限 3 次账号切换）。删号原因带上当时的余额与生效下限，
写进 `/manage` 底部的「自动删号记录」。

**流式请求的换号与截断**：响应头延迟到第一次真正下发内容时才写。

| 发现时机 | 行为 |
| --- | --- |
| 还没下发任何字节 | 删号 → 换账号 → 重新请求，调用方收到的是完整流，完全无感 |
| 已下发部分内容 | 已发出的内容撤不回来，因此立刻**截断**：补一个 `finish_reason: "length"` 的 chunk 再发 `data: [DONE]`，同时删号；下一次请求就落到健康账号 |

截断路径的关键是不让调用方干等或读到半截 SSE——标准 OpenAI 客户端收到 `[DONE]` 会正常结束读取。

## 管理接口

**分组**

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET / POST | `/admin/groups` | 列表 / 创建（重名 409，上限 200） |
| GET | `/admin/groups/{id}` | 分组详情含账号列表 |
| POST | `/admin/groups/{id}/refresh` | 刷新组内全部账号余额 |
| POST / PATCH | `/admin/groups/{id}/enable` | 启用 / 禁用分组 |
| DELETE | `/admin/groups/{id}` | 删除分组（级联删账号与 API） |

**账号**

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET / POST | `/admin/accounts` | 列表（含删号记录与汇总）/ 创建 |
| GET | `/admin/accounts/totals` | 全站汇总 |
| POST | `/admin/accounts/refresh-all` | 刷新全部账号余额 |
| DELETE | `/admin/accounts/batch` | 批量删号（体 `{"ids":[...]}`） |
| GET | `/admin/accounts/{id}` 或 `/balance` | 只读余额视图 |
| POST | `/admin/accounts/{id}/refresh` | 手动刷新单个账号 |
| DELETE | `/admin/accounts/{id}` | 删除账号 |
| PATCH | `/admin/accounts/{id}` | **恒 405**，配置保存后不可修改 |

**网关密钥**

`GET|POST /admin/keys`、`POST /admin/keys/bulk`、`DELETE /admin/keys/batch`、`GET /admin/keys/{id}/reveal`、`POST /admin/keys/{id}/reset-usage`、`PATCH|DELETE /admin/keys/{id}`

**管理员账户**（仅超管）

`GET|POST /admin/users`、`POST /admin/users/{id}/password`、`POST|PATCH /admin/users/{id}/enable`、`DELETE /admin/users/{id}`

**其他**

`POST /admin/setup`（免鉴权，仅未初始化时可用）、`POST /admin/login`、`POST /admin/logout`、
`GET /admin/session`、`POST /admin/password`、`GET /admin/dashboard`、
`GET|PATCH /admin/settings`、`POST /admin/models/probe`（拉上游模型列表）

除 `/admin/setup` / `login` / `logout` / `session` 外全部要求登录；
`/admin/password` 与 `/admin/dashboard` 对两种角色开放，其余全部要求超管。

## 负载均衡

两级调度：

1. **密钥 → 账号**：已绑定且账号仍可用（分组启用 + 账号可用 + 名下有健康 API **且该账号提供本次请求的模型**）时保持粘性；否则重新挑选，排序依次为
   ① 每个可用上游承载的密钥数最少 → ② 绑定密钥数最少 → ③ 无限额度优先 → ④ 余额更高 → ⑤ 创建更早。
2. **账号 → 上游 API**：候选收敛到该账号名下，再按 `STRATEGY` 选：
   `weighted-random`（默认）/ `round-robin` / `least-latency` / `least-inflight` / `priority`。

**按模型选账号**：请求处理顺序是「验密钥 → 读 `model` → 分配账号」，账号池先按
`Provider.SupportsModel(model)` 过滤，因此请求 `claude-3-opus` 绝不会落到只挂 gpt 系列 Key 的账号上；
压力排序也只按「能承接该模型的上游数」计算，避免高估窄口账号的容量。
没有任何账号提供该模型时返回 503，文案点明模型名。

**声明过该模型的账号优先**：创建账号时不勾任何模型等于「不限」，这类账号会接收任何模型，
但「没设限制」并不代表它真的有这个模型。所以候选池分两档：

| 档位 | 判定 | 用途 |
| --- | --- | --- |
| 明确声明 | `Provider.ExplicitlySupportsModel`：模型名精确命中、通配符命中（`gpt-4*`）或别名命中 | 首选 |
| 兜底 | 模型列表留空的「什么都收」上游 | 无人声明时才用 |

只要有账号明确声明了这个模型，就只在这些账号里挑，哪怕兜底账号余额高得多。
同一个账号内部也套用同样的优先级：明确挂了该模型的上游 Key 排在不设限制的 Key 之前。

绑定账号只是「不提供这个模型」时**不解绑**：本次请求临时借用别的账号跑完，
`key.accountId` 保持不变，免得一次冷门模型请求把密钥永久迁走、打乱均摊。
相应地 `/v1/models` 返回的是分组内全部可用账号的模型并集，与这个自动换号行为保持一致。

失败的上游进入冷却并在 `MAX_RETRIES` 内故障转移；冷却中的 Key 不计入账号健康度——
一个账号即使挂了 5 个 Key，若全部在冷却中也不会继续被分配流量。
限流 `rateLimitPerMin` 与 Token 额度 `quotaTokens` 在网关侧强制。

## 安全设计

- 口令 PBKDF2-SHA256 240000 轮；账户不存在时也跑一次等价耗时的假校验，抹平时间侧信道。
- 超级管理员账户名 AES-256-GCM 加密落盘 + SHA-256 摘要索引，**不可反查**；不区分「账号不存在 / 被禁用 / 口令错误」，防账户枚举。
- 登录失败按来源 IP 限速：10 分钟 5 次锁 15 分钟；`/admin/setup` 同样受限速保护。
- 会话只在内存、只存令牌摘要；HttpOnly + Secure + SameSite=Strict，8 小时绝对过期 / 90 分钟空闲过期；改密或停用账户立即注销相关会话。
- Cookie 会话的写请求校验 `X-CSRF-Token`；Bearer 令牌调用不依赖 Cookie，不受 CSRF 影响。
- 数据文件里上游 API Key、网关密钥、access_token 全部 AES-256-GCM 加密，主密钥来自 `MASTER_KEY` 或随机生成的 `db.master.key`（`0600`）。
- 网关密钥按摘要 O(1) 校验，列表只给掩码；账号凭据保存后接口层面不可读取。
- CSP `default-src 'none'` + `script-src 'self'`，**无 `unsafe-inline`**，所有页面脚本都是独立 `.js`；DOM 全部用 `textContent` 构建，无 HTML 字符串拼接。
- 同时下发 `X-Frame-Options: DENY`、`nosniff`、`no-referrer`、`Cross-Origin-Opener-Policy`、`Permissions-Policy`。
- 静态资源按后缀白名单放行，路径含 `..` 直接 400；未登录访问受限页跳登录页，不暴露页面结构。
- `/health` 只给聚合数量；`/v1/models` 的 `owned_by` 不暴露上游身份；启动横幅不打印任何账户名或口令。
- 二进制用 `-trimpath -ldflags '-s -w'` 构建，剥离符号表与源码路径，前端资源随二进制内嵌。
- 请求体上限 16MB。**本服务不提供 TLS**，公网必须放反向代理后面，并把 `ALLOW_ORIGIN` 收紧成自己的域名。

## 性能取向

- 高频统计走内存累加 + 30 秒合并落盘，请求路径不做同步写盘。
- 请求时刷新按账号做 singleflight，突发流量不会把额度接口放大成 N 倍请求。
- 后台只扫「到期」账号，间隔为 0 的账号不产生任何上游请求；无限额度账号完全不打上游。
- 并发刷新上限 6，先并发查网络再串行写回，避免 goroutine 争抢写锁。
- 无外部依赖（纯标准库），无数据库进程，常驻内存与 CPU 占用都很低。

## 数据迁移

`db.json` 带 `version` 字段（当前 4），启动时自动归一化：

- `version < 3`：补齐请求时刷新字段并**默认开启** `refreshOnRequest`（60 秒），升级后无需手工改配置。
- `version < 4`：旧分组默认 `enabled = true`；`config.users` 补成空数组（已有账户则标记为已初始化）。
- 缺失或越界的 `requestRefreshSec` 回落默认 60、上限截断 3600。

迁移是单向的，跨版本升级前先备份。

## 开发

需要 Go 1.26+（见 `go.mod`）。CI 在每次 push 与 PR 上跑同样这三条加双平台编译。

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
```

接口级冒烟，38 项断言（Windows；会重置本地数据并留下一套演示数据）：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\smoke-local.ps1
```

页面截图（开发期工具，自带极简 CDP over WebSocket 客户端，不参与服务端运行）：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\start-chrome.ps1
go run .\tools\devshot -user "<超管账户>" -password "<口令>" -pages "/dashboard,/manage" -theme dark -out _preview
```

本地脚本一览：`scripts/build.sh`（Linux 编译）、`scripts/install-linux.sh`（安装 / 升级）、
`scripts/deploy-from-github.sh`（远程一键部署）、`scripts/build.ps1` / `start-local.ps1` /
`stop-local.ps1` / `smoke-local.ps1` / `start-chrome.ps1`（Windows 开发）、
`scripts/pack-src.ps1`（打不含数据的源码包）、`scripts/publish-github.ps1`（发布到 GitHub）。

Go 测试覆盖：账号构建与余额判定、$0.50 安全线的边界判定、请求时刷新窗口、5 个 API 上限、
分组隔离与启停、密钥到账号的均摊分配、按模型选账号（含声明优先于兜底、临时换号不改写常驻绑定）、
账号内不跨账号轮转、余额耗尽自动删号与流量故障转移、
上游「余额不足」与「预扣费额度失败」两类文案的删号换号、
HTTP 200 body 内 `error` 与流式 SSE `error` 两条余额不足路径（含流中途截断收尾）、
无限额度账号不打上游、
请求时刷新触发自动切号与密钥重绑、分组级手动刷新、无可用账号 503、
New API 余额接口解析与回落、5 种均衡策略与冷却、SSE 流式转换、
`/v1/models` 规范格式（字段数 / 排序 / 通配符剔除 / 单模型 / 404 / 401 / 白名单收窄）、
`/setup` 初始化流程、角色越权拦截、管理员账户增删改、端到端页面与静态资源、落盘密文校验。
