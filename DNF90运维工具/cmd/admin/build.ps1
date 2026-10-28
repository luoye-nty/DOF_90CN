$ErrorActionPreference = "Stop"

$root = Resolve-Path (Join-Path $PSScriptRoot "..\..")
Push-Location $root
try {
    New-Item -ItemType Directory -Force -Path "_build" | Out-Null
    $env:CGO_ENABLED = "0"
    $env:GOOS = "linux"

    $env:GOARCH = "amd64"
    & go build -trimpath -ldflags "-s -w" -o _build\dnf90admin-linux-amd64 ./cmd/admin
    $env:GOARCH = "arm64"
    & go build -trimpath -ldflags "-s -w" -o _build\dnf90admin-linux-arm64 ./cmd/admin

    Get-FileHash _build\dnf90admin-linux-amd64, _build\dnf90admin-linux-arm64 -Algorithm SHA256 |
        Format-Table -AutoSize
} finally {
    Pop-Location
}
