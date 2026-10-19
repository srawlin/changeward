#!/usr/bin/env bash
# Build a release tarball for the current platform into dist/:
#   dist/changegate_<os>_<arch>.tar.gz
# pg_query_go uses cgo, so each platform is built natively (see release.yml).
set -euo pipefail
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m); case "$arch" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; esac
mkdir -p dist/stage
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION#v}" -o dist/stage/changegate ./cmd/changegate
cp LICENSE dist/stage/
tar -C dist/stage -czf "dist/changegate_${os}_${arch}.tar.gz" changegate LICENSE
rm -rf dist/stage
echo "dist/changegate_${os}_${arch}.tar.gz"
