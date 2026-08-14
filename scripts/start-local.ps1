# 后台启动本地预览实例（不阻塞当前终端，也不占用调用方的标准输出句柄）。
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\Claude\laskah\scripts\start-local.ps1
#
# 停止：powershell -NoProfile -File D:\Claude\laskah\scripts\stop-local.ps1

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$exe = Join-Path $root 'bin\laskah.exe'
$dataDir = Join-Path $root 'data'

if (-not (Test-Path -LiteralPath $exe)) {
  throw ("未找到 " + $exe + "，请先执行 scripts\build.ps1")
}
New-Item -ItemType Directory -Path $dataDir -Force | Out-Null

Get-Process -Name laskah -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 500

$env:HOST = '127.0.0.1'
$env:PORT = '8787'
$env:DATA_FILE = Join-Path $dataDir 'db.json'
$env:ALLOW_ORIGIN = 'http://127.0.0.1:8787'

$stdout = Join-Path $dataDir 'stdout.log'
$stderr = Join-Path $dataDir 'stderr.log'

# 直接 Start-Process -RedirectStandardOutput 会让子进程继承调用方的管道句柄，
# 当本脚本的输出被管道接收时（例如 | Out-String）调用方会一直等不到 EOF。
# 因此先用一个「不重定向」的中转 powershell 起进程（UseShellExecute 不继承句柄），
# 再由它把日志重定向到文件，本脚本即可立即返回。
$inner = $(
  "Start-Process -FilePath '" + $exe + "'" +
  " -WindowStyle Hidden" +
  " -RedirectStandardOutput '" + $stdout + "'" +
  " -RedirectStandardError '" + $stderr + "'"
)
Start-Process -FilePath 'powershell.exe' -WindowStyle Hidden -ArgumentList $(
  '-NoProfile', '-ExecutionPolicy', 'Bypass', '-WindowStyle', 'Hidden', '-Command', $inner
) | Out-Null

$base = 'http://127.0.0.1:8787'
$ready = $false
for ($i = 0; $i -lt 50; $i++) {
  Start-Sleep -Milliseconds 300
  try {
    $health = Invoke-RestMethod -Uri ($base + '/health') -TimeoutSec 3
    if ($health.ok) { $ready = $true; break }
  } catch { }
}

if (-not $ready) {
  Write-Output '启动失败，stderr:'
  Get-Content -Raw -LiteralPath $stderr -ErrorAction SilentlyContinue
  throw '健康检查未通过'
}

$proc = Get-Process -Name laskah -ErrorAction SilentlyContinue | Select-Object -First 1
$procId = if ($proc) { $proc.Id } else { '?' }
Write-Output ('READY pid=' + $procId + ' ' + $base)
