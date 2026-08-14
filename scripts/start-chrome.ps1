# 启动无头 Chrome 的远程调试实例，供 tools/devshot 截图使用。
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\Claude\laskah\scripts\start-chrome.ps1
#
# 停止：Get-Process -Name chrome | Where-Object { $_.Path -like '*Chrome*' } | Stop-Process -Force

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$profile = Join-Path $root '_preview\profile'
New-Item -ItemType Directory -Path $profile -Force | Out-Null

$candidates = @(
  'C:\Program Files\Google\Chrome\Application\chrome.exe',
  'C:\Program Files (x86)\Google\Chrome\Application\chrome.exe',
  'C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe',
  'C:\Program Files\Microsoft\Edge\Application\msedge.exe'
)
$browser = $candidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
if (-not $browser) { throw '未找到 Chrome 或 Edge' }

$args = @(
  '--headless=new',
  '--disable-gpu',
  '--hide-scrollbars',
  '--no-first-run',
  '--no-default-browser-check',
  '--remote-debugging-port=9222',
  ('--user-data-dir=' + $profile),
  'about:blank'
)
Start-Process -FilePath $browser -WindowStyle Hidden -ArgumentList $args | Out-Null

$ready = $false
for ($i = 0; $i -lt 40; $i++) {
  Start-Sleep -Milliseconds 300
  try {
    $version = Invoke-RestMethod -Uri 'http://127.0.0.1:9222/json/version' -TimeoutSec 3
    if ($version) { $ready = $true; break }
  } catch { }
}
if (-not $ready) { throw '调试端口 9222 未就绪' }

Write-Output ('CHROME READY ' + $browser)
