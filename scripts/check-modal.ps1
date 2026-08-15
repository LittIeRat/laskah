# 验证弹窗交互：在输入框里拖选文本不应关闭弹窗，点遮罩空白处仍应关闭。
#
# 依赖本地实例与开了远程调试的 Chrome：
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\start-local.ps1
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\start-chrome.ps1
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\check-modal.ps1
#
# 合成 PointerEvent 复现不了浏览器把 click 派发到共同祖先的行为，
# 所以这里让 devshot 通过 CDP 的 Input 域派发真实鼠标事件。
# 注入的 JS 通过临时文件交给 devshot：直接当命令行参数传会被 PowerShell 去掉引号。

param(
  [string]$Base = 'http://127.0.0.1:8787',
  [string]$User = 'Digital Gleam',
  [string]$Password = 'Laskah-2026-Super'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$env:GOROOT = 'D:\Claude\tools\go'
$env:GOPATH = 'D:\Claude\tools\gopath'
$env:GOCACHE = 'D:\Claude\tools\gocache'
$env:GOMODCACHE = 'D:\Claude\tools\gopath\pkg\mod'
$env:GOTMPDIR = 'D:\Claude\tools\gotmp'
$env:PATH = 'D:\Claude\tools\go\bin;' + $env:PATH

$work = Join-Path $root '_preview'
New-Item -ItemType Directory -Path $work -Force | Out-Null

$pass = 0
$fail = 0

function Check {
  param([string]$Name, [string]$Output, [string]$Needle)
  # devshot 打印的是 JSON 字符串字面量，里面的引号带反斜杠转义，先去掉再比对。
  $Output = $Output.Replace('\"', '"')
  if ($Output -like ('*' + $Needle + '*')) {
    $script:pass++
    Write-Output ('  PASS  ' + $Name)
  } else {
    $script:fail++
    Write-Output ('  FAIL  ' + $Name + ' :: 期望包含 ' + $Needle)
    Write-Output ('        实际 ' + $Output)
  }
}

function Shot {
  param([string]$Suffix, [string]$Open, [string]$Drag, [string]$After)
  # 脚本必须走文件：PowerShell 传给原生命令时会吞掉字符串里的双引号。
  # devshot 的 -eval-file / -after-file 会自动剥掉 UTF-8 BOM。
  $openFile = Join-Path $work ('open' + $Suffix + '.js')
  $afterFile = Join-Path $work ('after' + $Suffix + '.js')
  Set-Content -LiteralPath $openFile -Value $Open -Encoding UTF8
  Set-Content -LiteralPath $afterFile -Value $After -Encoding UTF8
  $out = & go run ./tools/devshot -base $Base -user $User -password $Password -pages /dashboard -out $work -suffix $Suffix -eval-file $openFile -drag $Drag -after-file $afterFile 2>&1
  Remove-Item -LiteralPath $openFile -Force -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $afterFile -Force -ErrorAction SilentlyContinue
  return ($out -join ' ')
}

Write-Output '--- Laskah 弹窗交互验证 ---'

$openDrag = '(async () => { const wait = (ms) => new Promise(r => setTimeout(r, ms)); const area = document.createElement("textarea"); area.rows = 4; area.value = "sk-aaaaaaaaaaaaaaaaaaaa"; window.__area = area; window.LB.modal({ title: "拖选文本", body: area, confirmText: "确定", onConfirm: () => false }); await wait(220); return JSON.stringify({ opened: !!document.querySelector(".modal-overlay") }); })()'
$afterDrag = '(async () => { const wait = (ms) => new Promise(r => setTimeout(r, ms)); await wait(400); const area = window.__area; const picked = area ? area.value.substring(area.selectionStart, area.selectionEnd) : ""; return JSON.stringify({ stillOpen: !!document.querySelector(".modal-overlay"), selected: picked.length }); })()'

# 在输入框内部按下、拖到遮罩左上角松开：这正是「选文本却退出弹窗」的操作序列。
$dragOut = Shot '-modal-drag' $openDrag '600,460,40,40' $afterDrag
Check '拖选输入框文本后弹窗保持打开' $dragOut '"stillOpen":true'
Check '拖选确实选中了文本' $dragOut '"selected":14'

$openClick = '(async () => { const wait = (ms) => new Promise(r => setTimeout(r, ms)); const area = document.createElement("textarea"); area.value = "sk-aaaaaaaaaaaaaaaaaaaa"; window.LB.modal({ title: "点击遮罩", body: area, confirmText: "确定", onConfirm: () => false }); await wait(220); return JSON.stringify({ opened: !!document.querySelector(".modal-overlay") }); })()'
$afterClick = '(async () => { const wait = (ms) => new Promise(r => setTimeout(r, ms)); await wait(500); return JSON.stringify({ closed: !document.querySelector(".modal-overlay") }); })()'

$clickOut = Shot '-modal-backdrop' $openClick '40,40,40,40' $afterClick
Check '点击遮罩空白处仍关闭弹窗' $clickOut '"closed":true'

Write-Output ''
Write-Output ('--- 结果: PASS=' + $pass + ' FAIL=' + $fail + ' ---')
if ($fail -gt 0) { exit 1 }
