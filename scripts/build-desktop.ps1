<#
.SYNOPSIS
    构建 omni2api 桌面壳：单文件、无控制台窗口、带图标与 DPI 清单的 Windows GUI exe。

.DESCRIPTION
    三步，全部是 Go 工具链，不需要 Python / Pillow / ImageMagick / windres：

        1) go run ./tools/genicon -out .build/icon/app.ico
             图标由 internal/icon 用代码画出来（唯一一份图形代码）。
             仓库里没有 .ico/.png —— 这是一次性派生物，用完即弃。

        2) go run github.com/akavel/rsrc@v0.10.2 -arch amd64 ^
              -ico .build/icon/app.ico -manifest assets/icon/app.manifest ^
              -o cmd/desktop/rsrc.syso
             Go 编译器看不到 .ico/.manifest，只有包目录下的 .syso 才会被
             链接进 PE 的 .rsrc 段。放在 cmd/desktop/ 是必须的。

        3) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -H=windowsgui" ^
              -o dist/NaiBox.exe ./cmd/desktop

    每一步的参数都有理由：
      -trimpath       去掉本机绝对路径与 module 缓存路径（否则二进制里会带上
                      C:\Users\<name>\go\pkg\mod\... ，既泄露信息又不可复现）。
      -s -w           去符号表与 DWARF，体积约减 30%。
      -H=windowsgui   PE 子系统设成 GUI：双击不弹黑色控制台窗口。代价是
                      stdout/stderr 不再挂控制台，日志必须走文件或界面。

    脚本可重复执行：三个产物都是覆盖写，且 genicon 的输出是逐字节确定的。

.PARAMETER OutDir
    输出目录，相对仓库根。默认 dist。

.PARAMETER Console
    调试用：第 3 步不传 -H=windowsgui，保留控制台子系统，能直接看到
    fmt.Print 输出。这个模式下的产物**不是**发布件。

.EXAMPLE
    powershell -NoProfile -File scripts/build-desktop.ps1

.EXAMPLE
    powershell -NoProfile -File scripts/build-desktop.ps1 -Console

.NOTES
    本机只有 Windows PowerShell 5.1，没有 pwsh 7 —— 脚本按 5.1 的语法子集写，
    并保存为 UTF-8 **带 BOM**（5.1 读无 BOM 的中文会按 ANSI 解出乱码）。
    有 pwsh 7 时 `pwsh -File scripts/build-desktop.ps1` 同样可用。

    第 2 步首次运行要下载 rsrc 模块。国内网络走 goproxy.cn（脚本里设了
    GOPROXY，可被调用方的同名环境变量覆盖）。
#>
[CmdletBinding()]
param(
    [string]$OutDir = 'dist',
    [switch]$Console
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$exeName = 'NaiBox.exe'
$icoPath = Join-Path $repoRoot '.build/icon/app.ico'
$manifestPath = Join-Path $repoRoot 'assets/icon/app.manifest'
$sysoPath = Join-Path $repoRoot 'cmd/desktop/rsrc.syso'

# Invoke-Go 跑一条 go 命令，失败就抛人话。
#
# 为什么要包一层：Windows PowerShell 5.1 在 $ErrorActionPreference='Stop' 下会把
# 原生命令写到 stderr 的任何一行当成**终止性错误**抛出，于是用户看到一段
# NativeCommandError 转储，而不是下面那句"第几步失败了"。这里临时放行 stderr，
# 自己判 $LASTEXITCODE。
function Invoke-Go {
    param(
        [Parameter(Mandatory = $true)][string[]]$GoArgs,
        [Parameter(Mandatory = $true)][string]$What
    )
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & go @GoArgs 2>&1 | ForEach-Object { Write-Host $_ }
        $code = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $prev
    }
    if ($code -ne 0) {
        throw "$What 失败（退出码 $code）。先把上面 go 的报错修掉再重试。"
    }
}

Push-Location $repoRoot
try {
    # ---- 1. 工具链
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw @"
找不到 go 命令，无法构建桌面壳。

请先安装 Go（>= 1.22，与 go.mod 一致）：
    winget install --id GoLang.Go -e
安装后**重开一个终端**让 PATH 生效，然后确认：
    go version
"@
    }
    Write-Host "[1/5] 工具链：$((& go version) -join ' ')"

    if (-not (Test-Path -LiteralPath (Join-Path $repoRoot 'cmd/desktop') -PathType Container)) {
        throw "缺少入口目录 cmd/desktop（桌面壳的 main 包应位于此）。"
    }

    # ---- 2. 图标（代码 → ICO）
    Write-Host "[2/5] 生成图标：go run ./tools/genicon -out .build/icon/app.ico"
    Invoke-Go -What '生成图标 (tools/genicon)' -GoArgs @(
        'run', './tools/genicon', '-out', $icoPath
    )

    # ---- 3. 资源对象（ICO + manifest → .syso）
    #
    # GOPROXY 只在本次脚本进程内生效，不污染调用者的环境；调用方已经设了就不覆盖。
    if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn,https://proxy.golang.org,direct' }
    $rsrcArgs = @(
        'run', 'github.com/akavel/rsrc@v0.10.2',
        '-arch', 'amd64',
        '-ico', $icoPath,
        '-manifest', $manifestPath,
        '-o', $sysoPath
    )
    Write-Host "[3/5] 生成资源对象：rsrc → cmd/desktop/rsrc.syso"
    try {
        Invoke-Go -What '生成资源对象 (rsrc)' -GoArgs $rsrcArgs
    }
    catch {
        throw @"
$($_.Exception.Message)

rsrc 需要从模块代理下载。若本机离线，先在有网的环境预热一次模块缓存：
    `$env:GOPROXY = 'https://goproxy.cn,direct'
    go run github.com/akavel/rsrc@v0.10.2 -h
"@
    }
    if (-not (Test-Path -LiteralPath $sysoPath -PathType Leaf)) {
        throw "rsrc 报成功但没有产出 $sysoPath。"
    }
    $sysoSize = (Get-Item -LiteralPath $sysoPath).Length
    Write-Host "      $sysoPath  $sysoSize 字节"

    # ---- 4. 构建
    $ldflags = '-s -w'
    if (-not $Console) { $ldflags = "$ldflags -H=windowsgui" }

    $outDirFull = if ([System.IO.Path]::IsPathRooted($OutDir)) { $OutDir } else { Join-Path $repoRoot $OutDir }
    New-Item -ItemType Directory -Force -Path $outDirFull | Out-Null
    $outPath = Join-Path $outDirFull $exeName

    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'

    Write-Host "[4/5] 构建：CGO_ENABLED=0 go build -trimpath -ldflags=`"$ldflags`" -o $outPath ./cmd/desktop"
    Invoke-Go -What 'go build' -GoArgs @(
        'build', '-trimpath', "-ldflags=$ldflags", '-o', $outPath, './cmd/desktop'
    )
    if (-not (Test-Path -LiteralPath $outPath -PathType Leaf)) {
        throw "go build 报成功但没有产出 $outPath。"
    }

    # ---- 5. 自检 + 结果
    #
    # go build 返回 0 不代表图标进了 exe：.syso 没被链接器收进去时编译器一声
    # 不响。读一遍 PE 头，把"双击有没有黑框""图标在不在"变成可判定的断言。
    Write-Host "[5/5] 自检 PE："
    Invoke-Go -What '自检 (genicon -verify)' -GoArgs @('run', './tools/genicon', '-verify', $outPath)

    $file = Get-Item -LiteralPath $outPath
    $sha = (Get-FileHash -LiteralPath $outPath -Algorithm SHA256).Hash
    Write-Host ""
    Write-Host ("体积：{0:N2} MB（{1} 字节）" -f ($file.Length / 1MB), $file.Length)
    Write-Host "SHA256：$sha"
    Write-Host "产物：$($file.FullName)"
    if ($Console) {
        Write-Warning "本次是 -Console 调试模式（带控制台），不能作为发布件。"
    }
}
finally {
    Pop-Location
}
