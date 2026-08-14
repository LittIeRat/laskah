# 把 Laskah 发布到 GitHub：建仓 -> 改写文档里的仓库占位符 -> 提交 -> 推送。
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\Claude\laskah\scripts\publish-github.ps1 -Token ghp_xxx
#
# Token 需要 Personal Access Token（classic，勾 `repo` 与 `workflow` 两个 scope）：
#   https://github.com/settings/tokens/new?scopes=repo,workflow&description=laskah-publish
#
# GitHub 从 2021-08-13 起彻底停用密码做 Git 与 API 认证，所以只能用 Token。

[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string] $Token,
  [string] $Repo = 'laskah',
  [string] $Description = 'Laskah — OpenAI 兼容的 API 负载均衡网关，Go 单二进制，多账号自动分配与余额耗尽自动删号',
  [switch] $Private
)

$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$ProgressPreference = 'SilentlyContinue'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$headers = @{
  Authorization          = "Bearer $Token"
  Accept                 = 'application/vnd.github+json'
  'X-GitHub-Api-Version' = '2022-11-28'
  'User-Agent'           = 'laskah-publish'
}

function Gh {
  param([string] $Method, [string] $Url, $Body)
  $p = @{ Method = $Method; Uri = $Url; Headers = $headers; UseBasicParsing = $true; TimeoutSec = 30 }
  if ($null -ne $Body) {
    $p.ContentType = 'application/json; charset=utf-8'
    $p.Body = [Text.Encoding]::UTF8.GetBytes(($Body | ConvertTo-Json -Depth 6 -Compress))
  }
  (Invoke-WebRequest @p).Content | ConvertFrom-Json
}

# ---------- 1. 校验 Token 与身份 ----------
Write-Output '==> 校验 Token'
$me = Gh GET 'https://api.github.com/user'
$owner = $me.login
Write-Output ("    登录身份 $owner")

# ---------- 2. 建仓（已存在则复用） ----------
$full = "$owner/$Repo"
$exists = $false
try { Gh GET "https://api.github.com/repos/$full" | Out-Null; $exists = $true } catch { }

if ($exists) {
  Write-Output "==> 仓库已存在，复用 $full"
} else {
  Write-Output "==> 创建仓库 $full"
  Gh POST 'https://api.github.com/user/repos' @{
    name        = $Repo
    description = $Description
    private     = [bool] $Private
    has_issues  = $true
    has_wiki    = $false
    auto_init   = $false
  } | Out-Null
}

# ---------- 3. 把文档里的 OWNER 占位符换成真实用户名 ----------
Write-Output '==> 改写仓库地址占位符'
$targets = @(
  'README.md',
  'deploy/QUICKSTART-LINUX.md',
  'deploy/DEPLOY.md',
  'scripts/deploy-from-github.sh'
)
foreach ($f in $targets) {
  $p = Join-Path $root $f
  if (-not (Test-Path -LiteralPath $p)) { continue }
  $raw = [IO.File]::ReadAllText($p)
  $new = $raw.Replace('OWNER/laskah', "$owner/$Repo")
  if ($new -ne $raw) {
    # 保持原换行风格：.sh 必须 LF
    [IO.File]::WriteAllText($p, $new, (New-Object Text.UTF8Encoding $false))
    Write-Output "    $f"
  }
}

# ---------- 4. 提交前自检：绝不上传数据与密钥 ----------
git add -A | Out-Null
$staged = git diff --cached --name-only
$leak = $staged | Where-Object { $_ -match '(^|/)(data/|db\.json|db\.master\.key)' -or $_ -match '\.(exe|log)$' }
if ($leak) {
  $leak | ForEach-Object { Write-Output "泄漏: $_" }
  throw '发布中止：暂存区里出现了数据、密钥或二进制'
}
Write-Output ("==> 待推送文件 " + ($staged | Measure-Object).Count + " 个，自检通过")

# ---------- 5. 提交 ----------
$hasCommit = $true
try { git rev-parse --verify HEAD 2>$null | Out-Null; if ($LASTEXITCODE -ne 0) { $hasCommit = $false } } catch { $hasCommit = $false }

if ($staged) {
  $msg = if ($hasCommit) { 'docs: 文档填入真实仓库地址' } else { 'feat: Laskah API 负载均衡网关首次开源' }
  git commit -q -m $msg
  Write-Output "==> 已提交: $msg"
} else {
  Write-Output '==> 无改动可提交'
}

# ---------- 6. 推送 ----------
# Token 只出现在本次 push 的临时 URL 里：不写 remote、不写 .git/config、不进 credential store。
git remote remove origin 2>$null | Out-Null
git remote add origin "https://github.com/$full.git"
git branch -M main | Out-Null

Write-Output '==> 推送到 GitHub'
$authUrl = "https://${owner}:${Token}@github.com/$full.git"
git push $authUrl 'main:main' 2>&1 | ForEach-Object { $_ -replace [regex]::Escape($Token), '***' }
if ($LASTEXITCODE -ne 0) { throw '推送失败，见上面输出' }

# 上游指向不带凭据的 origin
git fetch origin main -q 2>$null | Out-Null
git branch --set-upstream-to=origin/main main 2>$null | Out-Null

# 自检：确认 Token 没有留在仓库配置里
$cfg = git config --local --get-regexp '.*' 2>$null
if ($cfg -and ($cfg -join "`n") -match [regex]::Escape($Token)) {
  throw '警告：Token 出现在 .git/config 里，请手动清除'
}
Write-Output '    Token 未写入仓库配置'

Write-Output ''
Write-Output "仓库地址   https://github.com/$full"
Write-Output "一键部署   curl -fsSL https://raw.githubusercontent.com/$full/main/scripts/deploy-from-github.sh | sudo bash"
