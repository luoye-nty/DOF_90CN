#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
mkdir -p _build

export CGO_ENABLED=0
export GOOS=linux

GOARCH=amd64 go build -trimpath -ldflags "-s -w" \
  -o _build/dnf90admin-linux-amd64 ./cmd/admin
GOARCH=arm64 go build -trimpath -ldflags "-s -w" \
  -o _build/dnf90admin-linux-arm64 ./cmd/admin

sha256sum _build/dnf90admin-linux-amd64 _build/dnf90admin-linux-arm64
