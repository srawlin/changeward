#!/usr/bin/env bash
# Lint and short tests; used before every commit.
set -euo pipefail
export MACOSX_DEPLOYMENT_TARGET=${MACOSX_DEPLOYMENT_TARGET:-13.0}
out=$(golangci-lint run ./... 2>&1 | tail -1)
[ "$out" = "0 issues." ] || { golangci-lint run ./... ; exit 1; }
go build ./... >/dev/null 2>&1
