$ErrorActionPreference = "Stop"
$Root = [IO.Path]::GetFullPath($PSScriptRoot)
$BuildDirectory = Join-Path $Root "build"
$RuntimeAssetDirectory = Join-Path $Root "DNF90.Setup.Builder\RuntimeAssets"
$InstallerProject = Join-Path $Root "DNF90.Setup.Installer\DNF90.Setup.Installer.csproj"
$BuilderProject = Join-Path $Root "DNF90.Setup.Builder\DNF90.Setup.Builder.csproj"
$InstallerExeName = "落叶DNF安装程序.exe"
$BuilderExeName = "落叶DNF安装包配置器.exe"
$WorkRoot = Join-Path ([IO.Path]::GetTempPath()) ("DNF90InstallerSuite-" + [Guid]::NewGuid().ToString("N"))
$InstallerPublish = Join-Path $WorkRoot "installer"
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
    New-Item -ItemType Directory -Path $InstallerPublish -Force | Out-Null
    New-Item -ItemType Directory -Path $BuilderPublish -Force | Out-Null

    Invoke-DotNet @(
        "publish", $InstallerProject,
        "-c", "Release",
        "-r", "win-x86",
        "--self-contained", "true",
        "-p:PublishSingleFile=true",
        "-p:DebugType=None",
        "-o", $InstallerPublish
    )

    $installerExe = Join-Path $InstallerPublish $InstallerExeName
    if (-not (Test-Path -LiteralPath $installerExe -PathType Leaf)) {
        throw "安装器发布产物不存在：$installerExe"
    }
    Copy-Item -LiteralPath $installerExe -Destination (Join-Path $RuntimeAssetDirectory $InstallerExeName) -Force

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
