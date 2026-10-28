@echo off
setlocal
chcp 65001 >nul
set "DNF_BUILD_SCRIPT=%~f0"
set "PS_ENC=JAB0AD0AWwBJAE8ALgBGAGkAbABlAF0AOgA6AFIAZQBhAGQAQQBsAGwAVABlAHgAdAAoACQAZQBuAHYAOgBEAE4ARgBfAEIAVQBJAEwARABfAFMAQwBSAEkAUABUACwAWwBUAGUAeAB0AC4ARQBuAGMAbwBkAGkAbgBnAF0AOgA6AFUAVABG"
set "PS_ENC=%PS_ENC%ADgAKQAKACQAaQA9ACQAdAAuAEkAbgBkAGUAeABPAGYAKAAiACMAIABQAE8AVwBFAFIAUwBIAEUATABMAF8AQwBPAEQARQBfAFMAVABBAFIAVAAjACIAKQAKAGkAZgAoACQAaQAgAC0AbAB0ACAAMAApAHsAdABoAHIAbwB3ACAAIgBQAG8A"
set "PS_ENC=%PS_ENC%dwBlAHIAUwBoAGUAbABsACAAbQBhAHIAawBlAHIAIABtAGkAcwBzAGkAbgBnACIAfQAKAEkAbgB2AG8AawBlAC0ARQB4AHAAcgBlAHMAcwBpAG8AbgAgACQAdAAuAFMAdQBiAHMAdAByAGkAbgBnACgAJABpACkA"

where pwsh.exe >nul 2>nul
if errorlevel 1 goto use_windows_powershell
pwsh.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -EncodedCommand %PS_ENC%
goto powershell_done

:use_windows_powershell
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -EncodedCommand %PS_ENC%

:powershell_done

set "BUILD_EXIT_CODE=%ERRORLEVEL%"
if not "%BUILD_EXIT_CODE%"=="0" pause
exit /b %BUILD_EXIT_CODE%

# POWERSHELL_CODE_START#
$ErrorActionPreference = "Stop"
$PSNativeCommandUseErrorActionPreference = $false
$Root = Split-Path -Parent $env:DNF_BUILD_SCRIPT
$LogRoot = Join-Path ([IO.Path]::GetTempPath()) "DNF90SourceBuild"

function Assert-BuildPath {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Parent
    )
    $full = [IO.Path]::GetFullPath($Path)
    $parentFull = [IO.Path]::GetFullPath($Parent).TrimEnd('\') + '\'
    if (-not $full.StartsWith($parentFull, [StringComparison]::OrdinalIgnoreCase)) {
        throw "拒绝操作非源码目录路径: $full"
    }
    return $full
}

function Resolve-Tool {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [string[]]$Candidates = @()
    )
    foreach ($candidate in $Candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate -PathType Leaf)) {
            return (Resolve-Path -LiteralPath $candidate).Path
        }
    }
    $command = Get-Command $Name -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -ne $command) {
        return $command.Source
    }
    throw "未找到构建工具: $Name"
}

function Invoke-Captured {
    param(
        [Parameter(Mandatory = $true)][string]$FileName,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$WorkingDirectory
    )
    Push-Location -LiteralPath $WorkingDirectory
    try {
        $lines = & $FileName @Arguments 2>&1
        $exitCode = $LASTEXITCODE
        $text = ($lines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
        return [pscustomobject]@{
            ExitCode = $exitCode
            Output   = $text
        }
    }
    finally {
        Pop-Location
    }
}

function Write-FailureLog {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Output
    )
    New-Item -ItemType Directory -Force -Path $LogRoot | Out-Null
    $safeName = $Name -replace '[\\/:*?"<>|]', '_'
    $logPath = Join-Path $LogRoot ($safeName + ".log")
    Set-Content -LiteralPath $logPath -Value $Output -Encoding UTF8
    return $logPath
}

function Replace-BuildArtifacts {
    param(
        [Parameter(Mandatory = $true)][string]$BuildDirectory,
        [Parameter(Mandatory = $true)][object[]]$Artifacts
    )
    $safeBuildDirectory = Assert-BuildPath -Path $BuildDirectory -Parent $Root
    New-Item -ItemType Directory -Force -Path $safeBuildDirectory | Out-Null
    Get-ChildItem -LiteralPath $safeBuildDirectory -Force | ForEach-Object {
        Remove-Item -LiteralPath $_.FullName -Recurse -Force
    }
    foreach ($artifact in $Artifacts) {
        Copy-Item -LiteralPath $artifact.Source -Destination (Join-Path $safeBuildDirectory $artifact.Name) -Force
    }
}

function Build-GoService {
    param(
        [Parameter(Mandatory = $true)][string]$DisplayName,
        [Parameter(Mandatory = $true)][string]$WorkingDirectory,
        [Parameter(Mandatory = $true)][string]$Package,
        [Parameter(Mandatory = $true)][string]$BuildDirectory,
        [Parameter(Mandatory = $true)][string]$OutputName
    )
    $go = Resolve-Tool -Name "go" -Candidates @(
        "D:\dof\90cn\tools\go\bin\go.exe",
        (Join-Path $env:TEMP "go-sdk-1.26.9\go\bin\go.exe")
    )
    $tempOutput = Join-Path ([IO.Path]::GetTempPath()) ("dnf90-build-" + [Guid]::NewGuid().ToString("N") + ".tmp")
    $oldGoos = $env:GOOS
    $oldGoarch = $env:GOARCH
    $oldCgo = $env:CGO_ENABLED
    try {
        $env:GOOS = "linux"
        $env:GOARCH = "amd64"
        $env:CGO_ENABLED = "0"
        $result = Invoke-Captured -FileName $go -WorkingDirectory $WorkingDirectory -Arguments @(
            "build", "-buildvcs=false", "-mod=readonly", "-trimpath",
            '-ldflags=-s -w', "-o", $tempOutput, $Package
        )
        if ($result.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $tempOutput -PathType Leaf)) {
            $log = Write-FailureLog -Name $DisplayName -Output $result.Output
            throw "构建失败，退出码 $($result.ExitCode)。日志: $log"
        }
        Replace-BuildArtifacts -BuildDirectory $BuildDirectory -Artifacts @(
            [pscustomobject]@{ Source = $tempOutput; Name = $OutputName }
        )
        return (Join-Path $BuildDirectory $OutputName)
    }
    finally {
        if (Test-Path -LiteralPath $tempOutput) {
            Remove-Item -LiteralPath $tempOutput -Force
        }
        $env:GOOS = $oldGoos
        $env:GOARCH = $oldGoarch
        $env:CGO_ENABLED = $oldCgo
    }
}

function Build-Launcher {
    $dotnet = Resolve-Tool -Name "dotnet"
    $workingDirectory = Join-Path $Root "登录器"
    $buildDirectory = Join-Path $workingDirectory "build"
    $tempDirectory = Join-Path ([IO.Path]::GetTempPath()) ("dnf90-launcher-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $tempDirectory | Out-Null
    try {
        $result = Invoke-Captured -FileName $dotnet -WorkingDirectory $workingDirectory -Arguments @(
            "publish", "DNFLogin.csproj", "-c", "Release", "-r", "win-x64",
            "--self-contained", "true", "-p:PublishSingleFile=true", "-o", $tempDirectory
        )
        $exe = Join-Path $tempDirectory "落叶DNF登录器.exe"
        if ($result.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $exe -PathType Leaf)) {
            $log = Write-FailureLog -Name "登录器" -Output $result.Output
            throw "构建失败，退出码 $($result.ExitCode)。日志: $log"
        }
        Replace-BuildArtifacts -BuildDirectory $buildDirectory -Artifacts @(
            [pscustomobject]@{ Source = $exe; Name = "落叶DNF登录器.exe" }
        )
        return (Join-Path $buildDirectory "落叶DNF登录器.exe")
    }
    finally {
        if (Test-Path -LiteralPath $tempDirectory) {
            Remove-Item -LiteralPath $tempDirectory -Recurse -Force
        }
    }
}

function Resolve-VisualCppToolset {
    param([Parameter(Mandatory = $true)][string]$MSBuildPath)

    $binDirectory = Split-Path -Parent $MSBuildPath
    $msbuildDirectory = Split-Path -Parent (Split-Path -Parent $binDirectory)
    $vcDirectory = Join-Path $msbuildDirectory "Microsoft\VC"
    if (-not (Test-Path -LiteralPath $vcDirectory -PathType Container)) {
        throw "未找到 Visual C++ 构建目录: $vcDirectory"
    }

    $available = @()
    foreach ($vcVersion in (Get-ChildItem -LiteralPath $vcDirectory -Directory | Sort-Object Name -Descending)) {
        $toolsetDirectory = Join-Path $vcVersion.FullName "Platforms\Win32\PlatformToolsets"
        if (-not (Test-Path -LiteralPath $toolsetDirectory -PathType Container)) {
            continue
        }
        foreach ($toolset in (Get-ChildItem -LiteralPath $toolsetDirectory -Directory)) {
            if ($available -notcontains $toolset.Name) {
                $available += $toolset.Name
            }
        }
    }

    foreach ($preferred in @("v143", "v142", "v141")) {
        if ($available -contains $preferred) {
            return $preferred
        }
    }
    if ($available.Count -gt 0) {
        return $available[0]
    }
    throw "未安装可用于 Win32 的 Visual C++ 工具集。"
}

function Build-90CNDll {
    $workingDirectory = Join-Path $Root "DNF90Server\client-patch\_op205gate_build"
    $project = Join-Path $workingDirectory "90CN.vcxproj"
    $buildDirectory = Join-Path $workingDirectory "build"
    $helperDirectory = Join-Path $Root "登录器\Helper"
    $tempDirectory = Join-Path ([IO.Path]::GetTempPath()) ("dnf90-90cn-" + [Guid]::NewGuid().ToString("N"))

    $vswhere = Join-Path ${env:ProgramFiles(x86)} "Microsoft Visual Studio\Installer\vswhere.exe"
    $msbuildCandidates = @(
        "D:\Software\Microsoft\VisualStudio\Product\MSBuild\Current\Bin\MSBuild.exe"
    )
    if (Test-Path -LiteralPath $vswhere -PathType Leaf) {
        $foundMsBuild = & $vswhere -latest -products * -requires Microsoft.Component.MSBuild `
            -find "MSBuild\**\Bin\MSBuild.exe" 2>$null | Select-Object -First 1
        if ($foundMsBuild) {
            $msbuildCandidates = @($foundMsBuild) + $msbuildCandidates
        }
    }
    $msbuild = Resolve-Tool -Name "MSBuild.exe" -Candidates $msbuildCandidates
    $platformToolset = Resolve-VisualCppToolset -MSBuildPath $msbuild

    New-Item -ItemType Directory -Path $tempDirectory | Out-Null
    try {
        $result = Invoke-Captured -FileName $msbuild -WorkingDirectory $workingDirectory -Arguments @(
            $project,
            "/t:Rebuild",
            "/p:Configuration=Release",
            "/p:Platform=Win32",
            "/p:PlatformToolset=$platformToolset",
            "/p:OutDir=$tempDirectory\",
            "/p:IntDir=$tempDirectory\obj\"
        )
        $dll = Join-Path $tempDirectory "90CN.dll"
        if ($result.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $dll -PathType Leaf)) {
            $log = Write-FailureLog -Name "90CN.dll" -Output $result.Output
            throw "构建失败，退出码 $($result.ExitCode)。日志: $log"
        }

        Replace-BuildArtifacts -BuildDirectory $buildDirectory -Artifacts @(
            [pscustomobject]@{ Source = $dll; Name = "90CN.dll" }
        )
        $artifact = Join-Path $buildDirectory "90CN.dll"
        $helper = Join-Path $helperDirectory "90CN.dll"
        Copy-Item -LiteralPath $artifact -Destination $helper -Force
        return [pscustomobject]@{
            Artifact = $artifact
            Helper   = $helper
        }
    }
    finally {
        if (Test-Path -LiteralPath $tempDirectory) {
            Remove-Item -LiteralPath $tempDirectory -Recurse -Force
        }
    }
}

function Show-ConfigurationHelp {
    $loginConfig = Join-Path $Root "登录器\DNFLogin.build.json"
    $authConfig = Join-Path $Root "DNF90运维工具\internal\launcherver\DNF90Ops.build.json"
    Write-Host "================ 落叶 DNF 90 服务构建工具 ================" -ForegroundColor Cyan
    Write-Host "登录器构建参数：" -ForegroundColor Yellow
    Write-Host "  文件: $loginConfig"
    Write-Host "  profileId       固定客户端版本，当前为 90"
    Write-Host "  launcherVersion 登录器版本号"
    Write-Host "  loginMode       登录模式：Remote / Local"
    Write-Host "  remoteIp        认证服务公网 IP"
    Write-Host "  remotePort      认证服务端口"
    Write-Host ""
    Write-Host "认证服务构建参数：" -ForegroundColor Yellow
    Write-Host "  文件: $authConfig"
    Write-Host "  launcherVersion 服务端要求的登录器版本，必须与登录器配置一致"
    Write-Host ""
    Write-Host "90CN.dll 构建说明：" -ForegroundColor Yellow
    Write-Host "  源码: DNF90Server\client-patch\_op205gate_build\90CN.vcxproj"
    Write-Host "  配置: Release / Win32"
    Write-Host "  充值页: 客户端根目录 90CN.ini 的 [web] shop_url 覆盖源码默认地址"
    Write-Host "==========================================================" -ForegroundColor Cyan
    Write-Host ""
}

function Clear-BuildScreen {
    try { Clear-Host } catch { }
}

function Wait-ReturnToMenu {
    while ($true) {
        $value = Read-Host "输入 0 返回一级菜单"
        if ($value.Trim() -eq "0") { return }
    }
}

function Invoke-BuildChoice {
    param([Parameter(Mandatory = $true)][string]$Choice)
    $displayName = ""
    $helperOutput = $null
    try {
        switch ($Choice) {
            "1" {
                $displayName = "服务端"
                $output = Build-GoService -DisplayName $displayName `
                    -WorkingDirectory (Join-Path $Root "DNF90Server\go-server") `
                    -Package "./cmd/server/dnf90" `
                    -BuildDirectory (Join-Path $Root "DNF90Server\go-server\build") `
                    -OutputName "dnf90server"
            }
            "2" {
                $displayName = "认证服务"
                $output = Build-GoService -DisplayName $displayName `
                    -WorkingDirectory (Join-Path $Root "DNF90运维工具") `
                    -Package "./cmd/serve" `
                    -BuildDirectory (Join-Path $Root "DNF90运维工具\cmd\serve\build") `
                    -OutputName "dnf90auth"
            }
            "3" {
                $displayName = "GM工具"
                $output = Build-GoService -DisplayName $displayName `
                    -WorkingDirectory (Join-Path $Root "DNF90运维工具") `
                    -Package "./cmd/admin" `
                    -BuildDirectory (Join-Path $Root "DNF90运维工具\cmd\admin\build") `
                    -OutputName "dnf90admin"
            }
            "4" {
                $displayName = "90CN.dll"
                $buildResult = Build-90CNDll
                $output = $buildResult.Artifact
                $helperOutput = $buildResult.Helper
            }
            "5" {
                $displayName = "登录器"
                $output = Build-Launcher
            }
            default { throw "未知构建选项: $Choice" }
        }
        Write-Host ""
        Write-Host "构建成功" -ForegroundColor Green
        Write-Host "产物路径: $output" -ForegroundColor Green
        if ($helperOutput) {
            Write-Host "登录器嵌入副本: $helperOutput" -ForegroundColor Green
            Write-Host "如需发布，请继续选择 5 - 登录器更新内嵌版本。" -ForegroundColor Yellow
        }
    }
    catch {
        Write-Host ""
        Write-Host "$displayName 构建失败" -ForegroundColor Red
        Write-Host $_.Exception.Message -ForegroundColor Red
    }
    Write-Host ""
    Wait-ReturnToMenu
}

while ($true) {
    Clear-BuildScreen
    Show-ConfigurationHelp
    Write-Host "请选择要构建的服务：" -ForegroundColor White
    Write-Host "  1 - 服务端"
    Write-Host "  2 - 认证服务"
    Write-Host "  3 - GM工具"
    Write-Host "  4 - 90CN.dll"
    Write-Host "  5 - 登录器"
    Write-Host "  0 - 退出"
    Write-Host ""
    $choice = (Read-Host "请输入选项").Trim()
    switch ($choice) {
        "0" { return }
        { $_ -in @("1", "2", "3", "4", "5") } {
            Clear-BuildScreen
            Write-Host "正在构建..." -ForegroundColor Yellow
            Invoke-BuildChoice -Choice $choice
        }
        default {
            Write-Host "无效选项，请输入 0、1、2、3、4 或 5。" -ForegroundColor Red
            Start-Sleep -Seconds 1
        }
    }
}
