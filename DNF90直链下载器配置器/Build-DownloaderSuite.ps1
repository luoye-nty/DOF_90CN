$ErrorActionPreference = "Stop"
$Root = [IO.Path]::GetFullPath($PSScriptRoot)
$BuildDirectory = Join-Path $Root "build"
$RuntimeAssetDirectory = Join-Path $Root "DNF90.DownloaderBuilder\RuntimeAssets"
$RuntimeProject = Join-Path $Root "DNF90.DirectDownloader\DNF90.DirectDownloader.csproj"
$BuilderProject = Join-Path $Root "DNF90.DownloaderBuilder\DNF90.DownloaderBuilder.csproj"
$RuntimeExeName = "DNF直链下载器.exe"
$BuilderExeName = "DNF直链下载器配置器.exe"
$WorkRoot = Join-Path ([IO.Path]::GetTempPath()) ("DNF90DownloaderSuite-" + [Guid]::NewGuid().ToString("N"))
$RuntimePublish = Join-Path $WorkRoot "runtime"
$BuilderPublish = Join-Path $WorkRoot "builder"

function Invoke-DotNet {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    & dotnet @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "dotnet 命令失败，退出码 $LASTEXITCODE"
    }
}

try {
    New-Item -ItemType Directory -Path $BuildDirectory -Force | Out-Null
    New-Item -ItemType Directory -Path $RuntimeAssetDirectory -Force | Out-Null
    New-Item -ItemType Directory -Path $RuntimePublish -Force | Out-Null
    New-Item -ItemType Directory -Path $BuilderPublish -Force | Out-Null

    Invoke-DotNet @(
        "publish", $RuntimeProject,
        "-c", "Release",
        "-r", "win-x86",
        "--self-contained", "true",
        "-p:PublishSingleFile=true",
        "-p:DebugType=None",
        "-o", $RuntimePublish
    )

    $runtimeExe = Join-Path $RuntimePublish $RuntimeExeName
    if (-not (Test-Path -LiteralPath $runtimeExe -PathType Leaf)) {
        throw "下载器发布产物不存在：$runtimeExe"
    }
    Copy-Item -LiteralPath $runtimeExe -Destination (Join-Path $RuntimeAssetDirectory $RuntimeExeName) -Force

    Invoke-DotNet @(
        "publish", $BuilderProject,
        "-c", "Release",
        "-r", "win-x86",
        "--self-contained", "true",
        "-p:PublishSingleFile=true",
        "-p:DebugType=None",
        "-o", $BuilderPublish
    )

    $builderExe = Join-Path $BuilderPublish $BuilderExeName
    if (-not (Test-Path -LiteralPath $builderExe -PathType Leaf)) {
        throw "配置器发布产物不存在：$builderExe"
    }
    Copy-Item -LiteralPath $builderExe -Destination (Join-Path $BuildDirectory $BuilderExeName) -Force

    Write-Host ""
    Write-Host "构建完成：" -ForegroundColor Green
    Get-ChildItem -LiteralPath $BuildDirectory -Filter "*.exe" |
        Select-Object Name, Length, LastWriteTime |
        Format-Table -AutoSize
}
finally {
    if (Test-Path -LiteralPath $WorkRoot) {
        Remove-Item -LiteralPath $WorkRoot -Recurse -Force
    }
}
