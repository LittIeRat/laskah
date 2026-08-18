# Laskah 本地接口级冒烟验证（会重置本地预览数据后重新初始化）。
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\Claude\laskah\scripts\smoke-local.ps1
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\Claude\laskah\scripts\smoke-local.ps1 -KeepData
#
# 覆盖：初始化超管 -> 登录 -> 会话角色 -> 分组 -> 账号(无余额查询=无限额度)
#       -> 分组启停 -> 管理员账户分级 -> 网关密钥 -> 看板 -> /v1/models -> 权限隔离。
#
# 默认清空 data\db.json 与 data\db.master.key，跑完会留下一套可直接预览的演示数据。
# 加 -KeepData 则复用现有数据（此时分组/账号相关断言可能因重名而失败）。

param([switch]$KeepData)

$ErrorActionPreference = 'Stop'
$base = 'http://127.0.0.1:8787'
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)

if (-not $KeepData) {
  # 重置数据必须先停服务：db.json 由服务进程持有，删除后要重新拉起才会回到待初始化状态。
  Get-Process -Name laskah -ErrorAction SilentlyContinue | Stop-Process -Force
  Start-Sleep -Milliseconds 500
  Remove-Item -LiteralPath (Join-Path $root 'data\db.json') -Force -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath (Join-Path $root 'data\db.master.key') -Force -ErrorAction SilentlyContinue
}

if (-not (Get-Process -Name laskah -ErrorAction SilentlyContinue)) {
  # 用独立的 powershell 进程拉起，避免子进程句柄挂在当前管道上导致脚本不返回。
  $starter = Join-Path $root 'scripts\start-local.ps1'
  $output = & powershell -NoProfile -ExecutionPolicy Bypass -File $starter 2>&1
  Write-Output $output
}
$superUser = 'Digital Gleam'
$superPass = 'Laskah-2026-Super'
$adminUser = 'viewer'
$adminPass = 'Laskah-2026-View'
$adminId = ''

$pass = 0
$fail = 0

function Step {
  param([string]$Name, [scriptblock]$Body)
  try {
    $result = & $Body
    $script:pass++
    Write-Output ("  PASS  " + $Name + $(if ($result) { " :: " + $result } else { "" }))
  } catch {
    $script:fail++
    Write-Output ("  FAIL  " + $Name + " :: " + $_.Exception.Message)
  }
}

function Api {
  param(
    [string]$Method,
    [string]$Path,
    $Body = $null,
    $Session = $null,
    [string]$Csrf = '',
    [string]$Bearer = '',
    [int]$Expect = 0
  )
  $headers = @{}
  if ($Csrf) { $headers['X-CSRF-Token'] = $Csrf }
  if ($Bearer) { $headers['Authorization'] = 'Bearer ' + $Bearer }

  $args = @{
    Uri             = $base + $Path
    Method          = $Method
    UseBasicParsing = $true
    TimeoutSec      = 20
    Headers         = $headers
  }
  if ($null -ne $Body) {
    # PowerShell 5.1 默认按 latin-1 编码请求体，中文会变成问号，必须显式转成 UTF-8 字节。
    $args['Body'] = [System.Text.Encoding]::UTF8.GetBytes(($Body | ConvertTo-Json -Depth 8 -Compress))
    $args['ContentType'] = 'application/json; charset=utf-8'
  }
  if ($null -ne $Session) { $args['WebSession'] = $Session }

  try {
    $response = Invoke-WebRequest @args
    $status = [int]$response.StatusCode
    $text = $response.Content
  } catch [System.Net.WebException] {
    $raw = $_.Exception.Response
    if ($null -eq $raw) { throw }
    $status = [int]$raw.StatusCode
    $reader = New-Object System.IO.StreamReader($raw.GetResponseStream())
    $text = $reader.ReadToEnd()
    $reader.Close()
  }

  if ($Expect -gt 0 -and $status -ne $Expect) {
    throw ("HTTP " + $status + " 期望 " + $Expect + " :: " + $text)
  }
  $data = $null
  if ($text) { try { $data = $text | ConvertFrom-Json } catch { } }
  return [pscustomobject]@{ Status = $status; Text = $text; Data = $data }
}

Write-Output '--- Laskah 冒烟验证 ---'

Step 'GET /health' {
  $r = Api GET '/health' -Expect 200
  if (-not $r.Data.ok) { throw '健康检查未返回 ok' }
  'ok=true'
}

$needsSetup = (Api GET '/admin/setup' -Expect 200).Data.needsSetup

if ($needsSetup) {
  Step 'POST /admin/setup 创建超级管理员' {
    $r = Api POST '/admin/setup' @{ user = $superUser; password = $superPass; confirm = $superPass } -Expect 201
    if ($r.Data.data.role -ne 'super') { throw '角色不是 super' }
    'role=super id=' + $r.Data.data.id
  }
  Step 'POST /admin/setup 重复初始化被拒绝' {
    $r = Api POST '/admin/setup' @{ user = 'x'; password = 'Laskah-2026-X' } -Expect 409
    'HTTP 409'
  }
} else {
  Write-Output '  SKIP  已初始化，跳过 /admin/setup（如需重跑请删除 data\db.json 并重启）'
}

$superSession = $null
$superCsrf = ''

Step 'POST /admin/login 超管登录' {
  $headers = @{ 'Content-Type' = 'application/json' }
  $payload = [System.Text.Encoding]::UTF8.GetBytes((@{ user = $superUser; password = $superPass } | ConvertTo-Json -Compress))
  $r = Invoke-WebRequest -Uri ($base + '/admin/login') -Method POST -Body $payload -ContentType 'application/json; charset=utf-8' -SessionVariable sv -UseBasicParsing -TimeoutSec 20
  $script:superSession = $sv
  $json = $r.Content | ConvertFrom-Json
  $script:superCsrf = $json.csrfToken
  if (-not $json.isSuper) { throw 'isSuper 不为 true' }
  if ($json.home -ne '/dashboard') { throw 'home 不是 /dashboard' }
  'isSuper=true home=/dashboard'
}

Step 'POST /admin/login 错误口令被拒绝' {
  $r = Api POST '/admin/login' @{ user = $superUser; password = 'wrong-password' } -Expect 401
  'HTTP 401'
}

Step 'GET /admin/session 返回超管身份' {
  $r = Api GET '/admin/session' -Session $superSession -Expect 200
  if (-not $r.Data.isSuper) { throw '会话不是超管' }
  'user=' + $r.Data.user + ' role=' + $r.Data.role
}

Step '写请求缺少 CSRF 头被拒绝' {
  $r = Api POST '/admin/groups' @{ name = 'csrf-probe' } -Session $superSession -Expect 403
  'HTTP 403'
}

$groupA = ''
$groupB = ''

Step 'POST /admin/groups 创建分组 A' {
  $r = Api POST '/admin/groups' @{ name = '主力分组'; note = '冒烟验证' } -Session $superSession -Csrf $superCsrf -Expect 201
  $script:groupA = $r.Data.data.id
  'id=' + $script:groupA
}

Step 'POST /admin/groups 创建分组 B' {
  $r = Api POST '/admin/groups' @{ name = '备用分组' } -Session $superSession -Csrf $superCsrf -Expect 201
  $script:groupB = $r.Data.data.id
  'id=' + $script:groupB
}

Step 'POST /admin/groups 重名分组被拒绝' {
  $r = Api POST '/admin/groups' @{ name = '主力分组' } -Session $superSession -Csrf $superCsrf -Expect 409
  'HTTP 409'
}

$accountA = ''

Step 'POST /admin/accounts 创建账号并批量导入 5 个 key（未配额度查询）' {
  $keys = @(1..5 | ForEach-Object { 'sk-smoke-' + $_ + '-0123456789abcdef' }) -join "`n"
  $payload = @{
    groupId        = $groupA
    name           = '账号一'
    baseUrl        = 'https://upstream.example.com/v1'
    keys           = $keys
    selectedModels = @('gpt-4o-mini', 'gpt-4o')
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 201
  $script:accountA = $r.Data.data.id
  if ($r.Data.data.apiCount -ne 5) { throw ('导入的 API 数量为 ' + $r.Data.data.apiCount + '，期望 5') }
  if ($r.Data.data.maxApiCount -ne 5) { throw ('单账号上限为 ' + $r.Data.data.maxApiCount + '，期望 5') }
  if ($r.Data.created -ne 5) { throw ('created=' + $r.Data.created) }
  if (-not $r.Data.data.unlimited) { throw '未配置额度查询时应为无限余额' }
  'id=' + $script:accountA + ' apiCount=5 unlimited=true'
}

Step '余额安全线 $0.50 强制生效（未自填最低余额）' {
  $r = Api GET '/admin/accounts' -Session $superSession -Expect 200
  $acct = $r.Data.data | Where-Object { $_.id -eq $accountA }
  if (-not $acct) { throw '账号列表里找不到刚创建的账号' }
  if ($acct.minBalance -ne 0) { throw ('minBalance=' + $acct.minBalance + '，期望 0') }
  if ([math]::Abs($acct.balanceFloor - 0.5) -gt 0.0001) { throw ('balanceFloor=' + $acct.balanceFloor + '，期望 0.5') }
  'minBalance=0 -> balanceFloor=0.5 USD'
}

Step '超过 5 个 API 的部分被拒绝' {
  $keys = @(1..7 | ForEach-Object { 'sk-overflow-' + $_ + '-0123456789abcdef' }) -join "`n"
  $payload = @{
    groupId = $groupA
    name    = '账号二'
    baseUrl = 'https://upstream.example.com/v1'
    keys    = $keys
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf
  if ($r.Status -ge 400) { return ('HTTP ' + $r.Status + ' 直接拒绝') }
  if ($r.Data.data.apiCount -ne 5) { throw ('实际导入 ' + $r.Data.data.apiCount + ' 个，期望被截到 5') }
  if ($r.Data.skipped.Count -ne 2) { throw ('skipped=' + $r.Data.skipped.Count + '，期望 2') }
  'apiCount=5 skipped=2 上限生效'
}

Step 'GET /admin/accounts 不回显任何凭据' {
  $r = Api GET '/admin/accounts' -Session $superSession -Expect 200
  if ($r.Text -match 'sk-smoke-') { throw '响应中出现上游 API Key 明文' }
  if ($r.Text -match 'accessToken"s*:s*"[^"]+"') { throw '响应中出现访问令牌' }
  '凭据未回显'
}

Step 'PATCH 账号配置不被允许（保存后只能查余额、启停或删除）' {
  $r = Api PATCH ('/admin/accounts/' + $accountA) @{ name = '改名尝试' } -Session $superSession -Csrf $superCsrf
  if ($r.Status -lt 400) { throw ('账号配置被修改成功，HTTP ' + $r.Status) }
  'HTTP ' + $r.Status
}

Step '账号频率限制：留空表示不限制' {
  $r = Api GET '/admin/accounts' -Session $superSession -Expect 200
  $acct = $r.Data.data | Where-Object { $_.id -eq $accountA }
  if ($null -ne $acct.rateLimitPerMin) { throw ('rateLimitPerMin=' + $acct.rateLimitPerMin + '，期望 null') }
  if ($acct.suspended) { throw '新建账号不应处于暂停状态' }
  'rateLimitPerMin=null suspended=false'
}

$rateLimitedAccount = ''

Step 'POST /admin/accounts 创建带频率限制的账号' {
  $payload = @{
    groupId         = $groupB
    name            = '限速账号'
    baseUrl         = 'https://upstream.example.com/v1'
    keys            = 'sk-ratelimit-0123456789abcdef'
    rateLimitPerMin = 30
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 201
  $script:rateLimitedAccount = $r.Data.data.id
  if ($r.Data.data.rateLimitPerMin -ne 30) { throw ('rateLimitPerMin=' + $r.Data.data.rateLimitPerMin + '，期望 30') }
  'rateLimitPerMin=30'
}

Step '频率限制为 0 被拒绝' {
  $payload = @{
    groupId         = $groupB
    name            = '非法限速'
    baseUrl         = 'https://upstream.example.com/v1'
    keys            = 'sk-badrate-0123456789abcdef'
    rateLimitPerMin = 0
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 400
  'HTTP 400'
}

Step '账号暂停后退出分配池且不删除数据' {
  $off = Api POST ('/admin/accounts/' + $rateLimitedAccount + '/enable') @{ enabled = $false } -Session $superSession -Csrf $superCsrf -Expect 200
  $acct = $off.Data.data
  if (-not $acct.suspended) { throw '暂停未生效' }
  if ($acct.usable) { throw '暂停后 usable 应为 false' }
  if ($acct.apiCount -ne 1) { throw ('暂停不应删除上游 API，apiCount=' + $acct.apiCount) }
  $list = Api GET '/admin/accounts' -Session $superSession -Expect 200
  $still = $list.Data.data | Where-Object { $_.id -eq $rateLimitedAccount }
  if (-not $still) { throw '暂停账号不应从列表消失' }
  'suspended=true usable=false apiCount=1'
}

Step '账号重新启用后恢复可用' {
  $on = Api POST ('/admin/accounts/' + $rateLimitedAccount + '/enable') @{ enabled = $true } -Session $superSession -Csrf $superCsrf -Expect 200
  $acct = $on.Data.data
  if ($acct.suspended) { throw '启用后仍处于暂停' }
  if (-not $acct.enabled) { throw '启用后 enabled 应为 true' }
  if (-not $acct.usable) { throw '启用后 usable 应为 true' }
  'suspended=false usable=true'
}

Step 'POST 账号手动刷新余额（无限额度不打上游）' {
  $r = Api POST ('/admin/accounts/' + $accountA + '/refresh') -Session $superSession -Csrf $superCsrf -Expect 200
  'HTTP 200'
}

$manualAccount = ''

Step 'POST /admin/accounts 创建手动余额 + 按量计价账号' {
  $payload = @{
    groupId        = $groupB
    name           = '手动计费账号'
    baseUrl        = 'https://upstream.example.com/v1'
    keys           = 'sk-manual-0123456789abcdef'
    billingMode    = 'per_mtoken'
    pricePerMToken = 2.5
    manualBalance  = $true
    initialBalance = 20
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 201
  $script:manualAccount = $r.Data.data.id
  $acct = $r.Data.data
  if ($acct.billingMode -ne 'per_mtoken') { throw ('billingMode=' + $acct.billingMode) }
  if ($acct.pricePerMToken -ne 2.5) { throw ('pricePerMToken=' + $acct.pricePerMToken) }
  if (-not $acct.manualBalance) { throw '手动余额未生效' }
  if ($acct.unlimited) { throw '手动余额账号不应被视为无限额度' }
  if ([math]::Abs($acct.balance - 20) -gt 0.0001) { throw ('初始余额=' + $acct.balance + '，期望 20') }
  'billingMode=per_mtoken price=2.5/Mtoken balance=20 unlimited=false'
}

Step '手动余额账号刷新走本地口径（不打上游）' {
  $r = Api POST ('/admin/accounts/' + $manualAccount + '/refresh') -Session $superSession -Csrf $superCsrf -Expect 200
  if ($r.Data.data.source -ne 'local') { throw ('source=' + $r.Data.data.source + '，期望 local') }
  if ($r.Data.data.unlimited) { throw '手动余额账号不应返回无限额度' }
  'source=local unlimited=false'
}

Step '计价方式非法被拒绝' {
  $payload = @{
    groupId     = $groupB
    name        = '非法计价'
    baseUrl     = 'https://upstream.example.com/v1'
    keys        = 'sk-badbilling-0123456789abcdef'
    billingMode = 'per_banana'
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 400
  'HTTP 400'
}

Step '自定义完整端点必须是绝对地址' {
  $payload = @{
    groupId = $groupB
    name    = '相对端点'
    baseUrl = 'https://upstream.example.com/v1'
    keys    = 'sk-badendpoint-0123456789abcdef'
    chatUrl = '/v1/chat/completions'
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 400
  'HTTP 400'
}

Step '自定义 chat / responses / models 完整地址被接受但不回显' {
  $payload = @{
    groupId      = $groupB
    name         = '自定义端点账号'
    baseUrl      = 'https://upstream.example.com/v1'
    keys         = 'sk-endpoint-0123456789abcdef'
    chatUrl      = 'https://upstream.example.com/openai/v1/chat/completions'
    responsesUrl = 'https://upstream.example.com/openai/v1/responses'
    modelsUrl    = 'https://upstream.example.com/openai/v1/models'
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 201
  $acct = $r.Data.data
  if (-not $acct.hasCustomChatUrl) { throw '未记录自定义 chat 地址' }
  if (-not $acct.hasCustomRespUrl) { throw '未记录自定义 responses 地址' }
  if (-not $acct.hasCustomModelsUrl) { throw '未记录自定义 models 地址' }
  if ($r.Text -match 'openai/v1/chat/completions') { throw '自定义端点被回显' }
  'hasCustomChatUrl/RespUrl/ModelsUrl=true 且不回显'
}

Step 'POST /admin/scripts/validate 合法脚本回显请求且遮蔽凭据' {
  $script1 = @'
({
  request: {
    url: "{{baseUrl}}/api/user/self",
    method: "GET",
    headers: {
      "Authorization": "Bearer {{accessToken}}",
      "New-Api-User": "{{userId}}"
    }
  },
  extractor: function (response) {
    if (response.success && response.data) {
      return {
        planName: response.data.group || "默认套餐",
        remaining: response.data.quota / 500000,
        used: response.data.used_quota / 500000,
        unit: "USD"
      };
    }
    return { isValid: false, invalidMessage: response.message || "查询失败" };
  }
})
'@
  $payload = @{
    script      = $script1
    queryUrl    = 'https://console.example.com'
    accessToken = 'tok-abcdef0123456789'
    userId      = '114514'
  }
  $r = Api POST '/admin/scripts/validate' $payload -Session $superSession -Csrf $superCsrf -Expect 200
  $d = $r.Data.data
  if (-not $d.ok) { throw ('合法脚本校验失败: ' + $d.error) }
  if ($d.method -ne 'GET') { throw ('method=' + $d.method) }
  if ($d.url -ne 'https://console.example.com/api/user/self') { throw ('url=' + $d.url) }
  if ($d.hasBody) { throw 'GET 脚本不应带请求体' }
  if ($r.Text -match 'abcdef0123456789') { throw '访问令牌被原文回显' }
  'ok=true ' + $d.method + ' ' + $d.url + ' 凭据已遮蔽'
}

Step 'POST /admin/scripts/validate 危险与残缺脚本被拒绝' {
  $bad = Api POST '/admin/scripts/validate' @{ script = '({ request: { url: "{{baseUrl}}/x", method: "GET" } })' } -Session $superSession -Csrf $superCsrf -Expect 200
  if ($bad.Data.data.ok) { throw '缺少 extractor 的脚本被判为合法' }
  if ($bad.Data.data.error -notmatch 'extractor') { throw ('错误信息未说明 extractor: ' + $bad.Data.data.error) }
  $danger = Api POST '/admin/scripts/validate' @{ script = '({ request: { url: require("http") } })' } -Session $superSession -Csrf $superCsrf -Expect 200
  if ($danger.Data.data.ok) { throw '含 require 的脚本被判为合法' }
  $unknown = Api POST '/admin/scripts/validate' @{ script = '({ request: { url: "{{secretKey}}/x", method: "GET" }, extractor: function (r) { return { remaining: 1 }; } })' } -Session $superSession -Csrf $superCsrf -Expect 200
  if ($unknown.Data.data.ok) { throw '未知模板变量被放过' }
  '缺 extractor / require / 未知变量 均被拒绝'
}

$scriptAccount = ''

Step 'POST /admin/accounts 创建脚本查询账号（自定义完整额度地址）' {
  $script2 = @'
({
  request: {
    url: "{{baseUrl}}/console/quota",
    method: "GET",
    headers: { "X-Token": "{{accessToken}}" }
  },
  extractor: function (response) {
    if (!response.ok) { return { isValid: false, invalidMessage: response.reason }; }
    return { planName: response.plan, remaining: response.left, used: response.spent, unit: "USD" };
  }
})
'@
  $payload = @{
    groupId     = $groupB
    name        = '脚本查询账号'
    baseUrl     = 'https://upstream.example.com/v1'
    keys        = 'sk-scripted-0123456789abcdef'
    queryUrl    = 'https://console.example.com'
    accessToken = 'tok-console-0123456789'
    queryScript = $script2
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 201
  $script:scriptAccount = $r.Data.data.id
  $acct = $r.Data.data
  if (-not $acct.hasQueryScript) { throw '未记录查询脚本' }
  if (-not $acct.hasQueryUrl) { throw '未记录额度查询完整地址' }
  if (-not $acct.hasBalanceQuery) { throw '配了脚本却未标记可查额度' }
  if ($acct.unlimited) { throw '配了脚本的账号不应视为无限余额' }
  if ($r.Text -match 'extractor') { throw '脚本源码被回显' }
  if ($r.Text -match 'tok-console') { throw '访问令牌被回显' }
  # 查询失败原因里允许出现请求地址（排查用），但它不能变成一个可读回的配置字段。
  foreach ($prop in $acct.PSObject.Properties) {
    if ($prop.Name -in @('checkError', 'scriptError')) { continue }
    if (([string]$prop.Value) -match 'console\.example\.com') { throw ('额度查询地址被字段 ' + $prop.Name + ' 回显') }
  }
  'id=' + $script:scriptAccount + ' hasQueryScript=true unlimited=false 且不回显脚本'
}

Step '脚本查询账号计入看板脚本口径统计' {
  $r = Api GET '/admin/dashboard' -Session $superSession -Expect 200
  $a = $r.Data.data.accounts
  if ($null -eq $a.scripted) { throw '缺少 scripted 计数' }
  if ($a.scripted -lt 1) { throw ('scripted=' + $a.scripted + '，期望至少 1') }
  if ($null -eq $a.scriptBroken) { throw '缺少 scriptBroken 计数' }
  if ($a.scriptBroken -ne 0) { throw ('scriptBroken=' + $a.scriptBroken + '，期望 0') }
  'scripted=' + $a.scripted + ' scriptBroken=0'
}

Step '非法脚本创建账号被拒绝' {
  $payload = @{
    groupId     = $groupB
    name        = '坏脚本账号'
    baseUrl     = 'https://upstream.example.com/v1'
    keys        = 'sk-badscript-0123456789abcdef'
    queryScript = '({ request: { url: 1 } })'
  }
  $r = Api POST '/admin/accounts' $payload -Session $superSession -Csrf $superCsrf -Expect 400
  'HTTP 400'
}

Step '额度查询完整地址必须是绝对地址（留空放行）' {
  $bad = @{
    groupId  = $groupB
    name     = '相对额度地址'
    baseUrl  = 'https://upstream.example.com/v1'
    keys     = 'sk-badquery-0123456789abcdef'
    queryUrl = 'console.example.com/api'
  }
  $r = Api POST '/admin/accounts' $bad -Session $superSession -Csrf $superCsrf -Expect 400
  $ok = @{
    groupId  = $groupB
    name     = '无额度接口账号'
    baseUrl  = 'https://upstream.example.com/v1'
    keys     = 'sk-noquery-0123456789abcdef'
    queryUrl = ''
  }
  $r2 = Api POST '/admin/accounts' $ok -Session $superSession -Csrf $superCsrf -Expect 201
  if (-not $r2.Data.data.unlimited) { throw '没有额度接口的账号应按无限余额处理' }
  if ($r2.Data.data.hasQueryUrl) { throw '留空却记录了额度查询地址' }
  '相对地址 400 / 留空 201 且 unlimited=true'
}

Step 'POST /admin/accounts/balance-query 查询总余额并拆分来源' {
  $r = Api POST '/admin/accounts/balance-query' -Session $superSession -Csrf $superCsrf -Expect 200
  $d = $r.Data.data
  if ($null -eq $d.queried) { throw '缺少 queried 计数' }
  if ($null -eq $d.failed) { throw '缺少 failed 计数' }
  if ($null -eq $d.totals.balance.queriedBalance) { throw '缺少上游查询余额小计' }
  if ($null -eq $d.totals.balance.manualAmount) { throw '缺少手动余额小计' }
  if ([math]::Abs($d.totals.balance.manualAmount - 20) -gt 0.0001) { throw ('manualAmount=' + $d.totals.balance.manualAmount + '，期望 20') }
  if ($null -eq $d.totals.tokens.selfMetered) { throw '缺少自算计量标记' }
  if (-not $d.groups) { throw '缺少分组汇总' }
  'queried=' + $d.queried + ' failed=' + $d.failed + ' manualAmount=' + $d.totals.balance.manualAmount
}

Step 'POST 分组手动刷新余额' {
  $r = Api POST ('/admin/groups/' + $groupA + '/refresh') -Session $superSession -Csrf $superCsrf -Expect 200
  'HTTP 200'
}

Step 'POST 分组禁用/启用' {
  $off = Api POST ('/admin/groups/' + $groupB + '/enable') @{ enabled = $false } -Session $superSession -Csrf $superCsrf -Expect 200
  if ($off.Data.data.enabled) { throw '禁用未生效' }
  $on = Api POST ('/admin/groups/' + $groupB + '/enable') @{ enabled = $true } -Session $superSession -Csrf $superCsrf -Expect 200
  if (-not $on.Data.data.enabled) { throw '启用未生效' }
  'enabled false->true 均生效'
}

$gatewayKey = ''

Step 'POST /admin/keys 创建网关密钥并自动分配账号' {
  $r = Api POST '/admin/keys' @{ name = 'smoke-key'; groupId = $groupA } -Session $superSession -Csrf $superCsrf -Expect 201
  $script:gatewayKey = $r.Data.data.key
  if (-not $script:gatewayKey) { throw '创建响应未返回明文密钥' }
  if (-not $r.Data.data.accountId) { throw '未自动分配账号' }
  'key=' + $r.Data.data.keyMasked + ' account=' + $r.Data.data.accountId
}

Step 'POST /admin/keys/bulk 批量创建 3 个密钥' {
  $r = Api POST '/admin/keys/bulk' @{ count = 3; template = @{ name = 'smoke-batch'; groupId = $groupA } } -Session $superSession -Csrf $superCsrf -Expect 201
  $count = $r.Data.data.Count
  if ($count -ne 3) { throw ('批量创建返回 ' + $count + ' 个') }
  'count=3'
}

Step 'GET /admin/keys 列表不回显明文密钥' {
  $r = Api GET '/admin/keys' -Session $superSession -Expect 200
  if ($r.Text.Contains($gatewayKey)) { throw '列表泄露明文密钥' }
  '仅返回掩码'
}

Step 'GET /admin/dashboard 汇总余额与用量' {
  $r = Api GET '/admin/dashboard' -Session $superSession -Expect 200
  $d = $r.Data.data
  if ($null -eq $d) { throw '看板未返回 data' }
  if ($null -eq $d.groups) { throw '看板缺少分组维度' }
  if ($d.groups.Count -lt 2) { throw ('看板分组数 ' + $d.groups.Count + '，期望 >= 2') }
  if ($null -eq $d.accounts) { throw '看板缺少账号汇总' }
  if ($d.accounts.apiCount -lt 5) { throw ('看板 apiCount=' + $d.accounts.apiCount) }
  if ($d.accounts.unlimited -lt 1) { throw '看板未统计无限额度账号' }
  if ($null -eq $d.tokens) { throw '看板缺少 tokens 汇总' }
  if ($null -eq $d.requests) { throw '看板缺少 requests 汇总' }
  if ($null -eq $d.balance) { throw '看板缺少 balance 汇总' }
  if ($null -eq $d.balance.total) { throw '看板缺少余额总量' }
  if ($d.keys.total -lt 4) { throw ('看板密钥数 ' + $d.keys.total + '，期望 >= 4') }
  $names = @($d.groups | ForEach-Object { $_.name }) -join ','
  'groups=' + $names + ' apiCount=' + $d.accounts.apiCount + ' unlimited=' + $d.accounts.unlimited
}

Step 'GET /v1/models 匿名返回公开模型目录' {
  $r = Api GET '/v1/models' -Expect 200
  if ($r.Data.object -ne 'list') { throw 'object 不是 list' }
  $ids = @($r.Data.data | ForEach-Object { $_.id })
  if ($ids -notcontains 'gpt-4o-mini') { throw '匿名目录缺少 gpt-4o-mini' }
  if (-not $r.Data.hint) { throw '匿名响应缺少 hint 提示' }
  # 外层再包一次 @()：Sort-Object -Unique 只剩一个值时会退化成标量字符串，直接索引会拿到首字符。
  $owners = @(@($r.Data.data | ForEach-Object { $_.owned_by }) | Sort-Object -Unique)
  if ($owners.Count -ne 1 -or $owners[0] -ne 'laskah') { throw ('匿名目录泄露了上游身份: ' + ($owners -join ',')) }
  'HTTP 200 models=' + ($ids -join ',') + ' owned_by=laskah'
}

Step 'GET /v1/models/{id} 匿名可查单个模型' {
  $r = Api GET '/v1/models/gpt-4o-mini' -Expect 200
  if ($r.Data.id -ne 'gpt-4o-mini') { throw ('id=' + $r.Data.id) }
  if ($r.Data.object -ne 'model') { throw ('object=' + $r.Data.object) }
  'id=gpt-4o-mini object=model'
}

Step 'GET /v1/models/{id} 匿名查询未知模型返回 404' {
  $r = Api GET '/v1/models/not-a-real-model' -Expect 404
  'HTTP 404'
}

Step 'GET /v1/models 携带无效密钥仍然 401' {
  $r = Api GET '/v1/models' -Bearer 'sk-not-a-real-key' -Expect 401
  'HTTP 401'
}

Step 'POST /v1/responses 无密钥返回 401' {
  $r = Api POST '/v1/responses' @{ model = 'gpt-4o-mini'; input = 'hi' } -Expect 401
  'HTTP 401'
}

Step 'POST /v1/responses 缺少 input 返回 400（路由已就位）' {
  $r = Api POST '/v1/responses' @{ model = 'gpt-4o-mini' } -Bearer $gatewayKey -Expect 400
  'HTTP 400'
}

Step 'POST /responses 与 /v1/responses 等价' {
  $r = Api POST '/responses' @{ model = 'gpt-4o-mini' } -Bearer $gatewayKey -Expect 400
  'HTTP 400（同一处理器）'
}

Step 'GET /v1/models 返回准确模型列表' {
  $r = Api GET '/v1/models' -Bearer $gatewayKey -Expect 200
  if ($r.Data.object -ne 'list') { throw 'object 不是 list' }
  $ids = @($r.Data.data | ForEach-Object { $_.id })
  if ($ids -notcontains 'gpt-4o-mini') { throw '缺少 gpt-4o-mini' }
  if ($ids -notcontains 'gpt-4o') { throw '缺少 gpt-4o' }
  $owner = @($r.Data.data | ForEach-Object { $_.owned_by }) | Select-Object -First 1
  'models=' + ($ids -join ',') + ' owned_by=' + $owner
}

Step 'POST /admin/users 创建普通管理员' {
  $r = Api POST '/admin/users' @{ user = $adminUser; password = $adminPass; role = 'admin'; note = '只看看板' } -Session $superSession -Csrf $superCsrf -Expect 201
  if ($r.Data.data.role -ne 'admin') { throw '角色不是 admin' }
  $script:adminId = $r.Data.data.id
  'role=admin id=' + $r.Data.data.id
}

$adminSession = $null
$adminCsrf = ''

Step 'POST /admin/login 普通管理员登录' {
  $headers = @{ 'Content-Type' = 'application/json' }
  $payload = [System.Text.Encoding]::UTF8.GetBytes((@{ user = $adminUser; password = $adminPass } | ConvertTo-Json -Compress))
  $r = Invoke-WebRequest -Uri ($base + '/admin/login') -Method POST -Body $payload -ContentType 'application/json; charset=utf-8' -SessionVariable av -UseBasicParsing -TimeoutSec 20
  $script:adminSession = $av
  $json = $r.Content | ConvertFrom-Json
  $script:adminCsrf = $json.csrfToken
  if ($json.isSuper) { throw '普通管理员被判为超管' }
  'isSuper=false home=' + $json.home
}

Step '管理员可读看板但不下发网关密钥' {
  $r = Api GET '/admin/dashboard' -Session $adminSession -Expect 200
  if ($r.Text.Contains($gatewayKey)) { throw '看板向管理员下发了明文密钥' }
  '看板可读，密钥已裁剪'
}

Step '管理员访问 /admin/groups 被 403' {
  $r = Api GET '/admin/groups' -Session $adminSession -Expect 403
  'HTTP 403'
}

Step '管理员访问 /admin/accounts 被 403' {
  $r = Api GET '/admin/accounts' -Session $adminSession -Expect 403
  'HTTP 403'
}

Step '管理员访问 /admin/keys 被 403' {
  $r = Api GET '/admin/keys' -Session $adminSession -Expect 403
  'HTTP 403'
}

Step '管理员访问 /admin/users 被 403' {
  $r = Api GET '/admin/users' -Session $adminSession -Expect 403
  'HTTP 403'
}

Step '管理员访问 /admin/scripts/validate 被 403' {
  $r = Api POST '/admin/scripts/validate' @{ script = '({ request: { url: "https://x.com", method: "GET" }, extractor: function (r) { return { remaining: 1 }; } })' } -Session $adminSession -Csrf $adminCsrf -Expect 403
  'HTTP 403'
}

Step '管理员改地址栏访问 /manage 被重定向到 /dashboard' {
  $r = Invoke-WebRequest -Uri ($base + '/manage') -WebSession $adminSession -UseBasicParsing -MaximumRedirection 0 -ErrorAction SilentlyContinue -TimeoutSec 20
  $status = [int]$r.StatusCode
  $location = $r.Headers['Location']
  if ($status -ne 302) { throw ('期望 302，实际 ' + $status) }
  if ($location -ne '/dashboard') { throw ('Location=' + $location) }
  '302 -> /dashboard'
}

Step '超管访问 /manage 返回页面' {
  $r = Invoke-WebRequest -Uri ($base + '/manage') -WebSession $superSession -UseBasicParsing -TimeoutSec 20
  if ([int]$r.StatusCode -ne 200) { throw ('HTTP ' + $r.StatusCode) }
  if (-not $r.Content.Contains('Laskah')) { throw '页面未包含品牌名' }
  'HTTP 200'
}

Step '/keys 永久重定向到 /dashboard' {
  $r = Invoke-WebRequest -Uri ($base + '/keys') -UseBasicParsing -MaximumRedirection 0 -ErrorAction SilentlyContinue -TimeoutSec 20
  $status = [int]$r.StatusCode
  if ($status -ne 301) { throw ('期望 301，实际 ' + $status) }
  if ($r.Headers['Location'] -ne '/dashboard') { throw ('Location=' + $r.Headers['Location']) }
  '301 -> /dashboard'
}

Step '匿名访问 /dashboard 被重定向到 /login' {
  $r = Invoke-WebRequest -Uri ($base + '/dashboard') -UseBasicParsing -MaximumRedirection 0 -ErrorAction SilentlyContinue -TimeoutSec 20
  if ([int]$r.StatusCode -ne 302) { throw ('期望 302，实际 ' + $r.StatusCode) }
  if ($r.Headers['Location'] -ne '/login') { throw ('Location=' + $r.Headers['Location']) }
  '302 -> /login'
}

Step '登录页与 logo 可访问' {
  $page = Invoke-WebRequest -Uri ($base + '/login') -UseBasicParsing -TimeoutSec 20
  if (-not $page.Content.Contains('logo.png')) { throw '登录页未引用 logo' }
  $logo = Invoke-WebRequest -Uri ($base + '/logo.png') -UseBasicParsing -TimeoutSec 20
  if ($logo.RawContentLength -le 0 -and $logo.Content.Length -le 0) { throw 'logo 为空' }
  'login 引用 logo，logo.png 可下载'
}

Step '安全响应头齐备（CSP 无 unsafe-inline）' {
  $r = Invoke-WebRequest -Uri ($base + '/login') -UseBasicParsing -TimeoutSec 20
  $csp = $r.Headers['Content-Security-Policy']
  if (-not $csp) { throw '缺少 CSP' }
  if ($csp.Contains('unsafe-inline')) { throw 'CSP 含 unsafe-inline' }
  if ($r.Headers['X-Frame-Options'] -ne 'DENY') { throw '缺少 X-Frame-Options: DENY' }
  if ($r.Headers['X-Content-Type-Options'] -ne 'nosniff') { throw '缺少 nosniff' }
  'CSP/XFO/nosniff 均就位'
}

Step '重置口令（尾随空格）后可用干净口令登录' {
  $reset = $adminPass + '-2  '
  $r = Api POST ('/admin/users/' + $adminId + '/password') @{ password = $reset; confirm = $reset } -Session $superSession -Csrf $superCsrf -Expect 200
  # 服务端与前端共用同一套归一化：带空格存进去，登录时不带空格也必须通过。
  $payload = [System.Text.Encoding]::UTF8.GetBytes((@{ user = $adminUser; password = ($adminPass + '-2') } | ConvertTo-Json -Compress))
  $login = Invoke-WebRequest -Uri ($base + '/admin/login') -Method POST -Body $payload -ContentType 'application/json; charset=utf-8' -SessionVariable rv -UseBasicParsing -TimeoutSec 20
  if ([int]$login.StatusCode -ne 200) { throw ('登录失败 HTTP ' + $login.StatusCode) }
  $json = $login.Content | ConvertFrom-Json
  $script:adminSession = $rv
  $script:adminCsrf = $json.csrfToken
  $script:adminPass = $adminPass + '-2'
  '重置后新口令可登录（首尾空白被忽略）'
}

Step '重置后旧口令立即失效' {
  $payload = [System.Text.Encoding]::UTF8.GetBytes((@{ user = $adminUser; password = 'Laskah-2026-View' } | ConvertTo-Json -Compress))
  try {
    $r = Invoke-WebRequest -Uri ($base + '/admin/login') -Method POST -Body $payload -ContentType 'application/json; charset=utf-8' -UseBasicParsing -TimeoutSec 20
    throw ('旧口令仍可登录 HTTP ' + [int]$r.StatusCode)
  } catch [System.Net.WebException] {
    $status = [int]$_.Exception.Response.StatusCode
    if ($status -ne 401) { throw ('期望 401，实际 ' + $status) }
  }
  'HTTP 401'
}
Step 'POST /admin/logout 注销后会话失效' {
  $out = Api POST '/admin/logout' -Session $adminSession -Csrf $adminCsrf -Expect 200
  $after = Api GET '/admin/session' -Session $adminSession -Expect 200
  if ($after.Data.authenticated) { throw '注销后会话仍有效' }
  'authenticated=false'
}

Write-Output ''
Write-Output ('--- 结果: PASS=' + $pass + ' FAIL=' + $fail + ' ---')
Write-Output ''
Write-Output '本地预览入口:'
Write-Output ('  登录     ' + $base + '/login')
Write-Output ('  数据看板 ' + $base + '/dashboard')
Write-Output ('  分组账号 ' + $base + '/manage')
Write-Output ('  超管账户 ' + $superUser + ' / ' + $superPass)
Write-Output ('  管理员   ' + $adminUser + ' / ' + $adminPass + '（已注销，可重新登录验证权限隔离）')

if ($fail -gt 0) { exit 1 }
