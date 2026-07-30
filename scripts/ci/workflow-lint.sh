#!/usr/bin/env bash
# GitHub workflow lint gate (Linux host). Keep this pin and invocation identical
# to workflow-lint.ps1 so local and hosted validation apply one schema contract.
set -euo pipefail
cd "$(dirname "$0")/../.."

ACTIONLINT='github.com/rhysd/actionlint/cmd/actionlint@v1.7.12'
HOST_OPERATING_SYSTEM="$(go env GOHOSTOS)"
HOST_ARCHITECTURE="$(go env GOHOSTARCH)"

SECONDS=0
echo "== workflow-lint =="
echo "-- actionlint v1.7.12 (all repository workflows)"

# go run must build for the host even when a caller has selected another GOOS.
# Empty external-linter commands keep the contract identical on every platform.
GOOS="$HOST_OPERATING_SYSTEM" GOARCH="$HOST_ARCHITECTURE" \
    go run "$ACTIONLINT" -shellcheck= -pyflakes=

echo "== workflow-lint: PASS in ${SECONDS}s =="
