# 本地预览：构建并启动 Laskah，等待健康检查通过。
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\Claude\laskah\scripts\run-local.ps1
#
# 停止：Get-Process -Name laskah | Stop-Process -Force

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$exe = Join-Path $root 'bin\laskah.exe'
$dataDir = Join-Path $root 'data'

if (-not (Test-Path -LiteralPath $exe)) {
  throw "未找到 $exe，请先执行 go build -trimpath -ldflags '-s -w' -o bin\laskah.exe .\cmd\laskah"
}
New-Item -ItemType Directory -Path $dataDir -Force | Out-Null

Get-Process -Name laskah -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 500

$env:HOST = '127.0.0.1'
$env:PORT = '8787'
$env:DATA_FILE = Join-Path $dataDir 'db.json'
$env:ALLOW_ORIGIN = 'http://127.0.0.1:8787'

$proc = Start-Process -FilePath $exe -PassThru -WindowStyle Hidden `
  -RedirectStandardOutput (Join-Path $dataDir 'stdout.log') `
  -RedirectStandardError (Join-Path $dataDir 'stderr.log')

$base = 'http://127.0.0.1:8787'
$ready = $false
for ($i = 0; $i -lt 30; $i++) {
  Start-Sleep -Milliseconds 400
  try {
    $health = Invoke-RestMethod -Uri ($base + '/health') -TimeoutSec 3
    if ($health.ok) { $ready = $true; break }
  } catch { }
}

if (-not $ready) {
  Write-Output '启动失败，stderr:'
  Get-Content -Raw -LiteralPath (Join-Path $dataDir 'stderr.log') -ErrorAction SilentlyContinue
  throw '健康检查未通过'
}

Write-Output ('READY pid=' + $proc.Id)
Write-Output ('初始化(首次启动先在此创建超级管理员) ' + $base + '/setup')
Write-Output ('登录     ' + $base + '/login')
Write-Output ('数据看板 ' + $base + '/dashboard')
Write-Output ('管理     ' + $base + '/manage')
