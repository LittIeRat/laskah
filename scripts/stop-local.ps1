# 停止本地预览实例。
Get-Process -Name laskah -ErrorAction SilentlyContinue | Stop-Process -Force
Write-Output '已停止 laskah'
