# 构建 Windows 本地二进制与 Linux 部署二进制。
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\Claude\laskah\scripts\build.ps1

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

# 正在运行的 bin\laskah.exe 会被占用，Go 只能把旧文件改名成 laskah.exe~ 留在目录里。
# 先停掉本地预览实例，保持产物目录干净。
Get-Process -Name laskah -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 400
Remove-Item -LiteralPath (Join-Path $root 'bin\laskah.exe~') -Force -ErrorAction SilentlyContinue

$env:GOROOT = 'D:\Claude\tools\go'
$env:GOPATH = 'D:\Claude\tools\gopath'
$env:GOCACHE = 'D:\Claude\tools\gocache'
$env:GOMODCACHE = 'D:\Claude\tools\gopath\pkg\mod'
$env:GOTMPDIR = 'D:\Claude\tools\gotmp'
$env:PATH = 'D:\Claude\tools\go\bin;' + $env:PATH
$env:CGO_ENABLED = '0'

$flags = '-s -w'

Write-Output '编译 windows/amd64 ...'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags $flags -o bin\laskah.exe .\cmd\laskah
if ($LASTEXITCODE -ne 0) { throw 'windows 构建失败' }

Write-Output '编译 linux/amd64 ...'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags $flags -o bin\laskah-linux-amd64 .\cmd\laskah
if ($LASTEXITCODE -ne 0) { throw 'linux/amd64 构建失败' }

Write-Output '编译 linux/arm64 ...'
$env:GOOS = 'linux'
$env:GOARCH = 'arm64'
go build -trimpath -ldflags $flags -o bin\laskah-linux-arm64 .\cmd\laskah
if ($LASTEXITCODE -ne 0) { throw 'linux/arm64 构建失败' }

Remove-Item Env:GOOS -ErrorAction SilentlyContinue
Remove-Item Env:GOARCH -ErrorAction SilentlyContinue

Get-ChildItem bin | Select-Object Name, Length | Format-Table -AutoSize

Write-Output ''
Write-Output 'SHA256:'
Get-ChildItem bin -File | ForEach-Object {
  $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash
  Write-Output ('  ' + $_.Name + '  ' + $hash)
}
