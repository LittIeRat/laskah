# 打包可分享的源码压缩包：只含源码与文档，排除 data/（凭据与主密钥）和 bin/（编译产物）。
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File D:\Claude\laskah\scripts\pack-src.ps1
#
# 产物：D:\Claude\laskah-src.zip

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$parent = Split-Path -Parent $root
$zip = Join-Path $parent 'laskah-src.zip'
$stageRoot = Join-Path $parent '_pack'
$stage = Join-Path $stageRoot 'laskah'

$excludeDirs = @('bin', 'data', '_preview', '_pack')

if (Test-Path -LiteralPath $stageRoot) { Remove-Item -LiteralPath $stageRoot -Recurse -Force }
New-Item -ItemType Directory -Path $stage -Force | Out-Null

$count = 0
Get-ChildItem -LiteralPath $root -Recurse -File -Force | ForEach-Object {
  $rel = $_.FullName.Substring($root.Length).TrimStart('\')
  $top = ($rel -split '\\')[0]
  if ($excludeDirs -contains $top) { return }
  if ($rel -eq '_report.txt') { return }
  if ($_.Extension -in @('.log', '.exe')) { return }
  $dst = Join-Path $stage $rel
  $dstDir = Split-Path -Parent $dst
  if (-not (Test-Path -LiteralPath $dstDir)) { New-Item -ItemType Directory -Path $dstDir -Force | Out-Null }
  Copy-Item -LiteralPath $_.FullName -Destination $dst -Force
  $count++
}

# 兜底自检：绝不允许把数据或密钥打进去。
$leak = Get-ChildItem -LiteralPath $stage -Recurse -Force |
  Where-Object { $_.Name -in @('db.json', 'db.master.key') -or $_.Extension -in @('.log', '.exe') }
if ($leak) {
  $leak | ForEach-Object { Write-Output ('泄漏: ' + $_.FullName) }
  throw '打包中止：暂存目录里出现了数据或密钥文件'
}

if (Test-Path -LiteralPath $zip) { Remove-Item -LiteralPath $zip -Force }
Compress-Archive -Path $stage -DestinationPath $zip -CompressionLevel Optimal
Remove-Item -LiteralPath $stageRoot -Recurse -Force

$item = Get-Item -LiteralPath $zip
Write-Output ('文件数  ' + $count)
Write-Output ('压缩包  ' + $item.FullName)
Write-Output ('体积    ' + [math]::Round($item.Length / 1KB, 1) + ' KB')
Write-Output ('SHA256  ' + (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash)
