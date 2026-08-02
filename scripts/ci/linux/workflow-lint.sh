#!/usr/bin/env bash
# GitHub workflow lint gate (Linux host). Keep this pin and invocation identical
# to workflow-lint.ps1 so local and hosted validation apply one schema contract.
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority

ACTIONLINT='github.com/rhysd/actionlint/cmd/actionlint@v1.7.12'
HOST_OPERATING_SYSTEM="$WINDSHARE_GO_HOST_OS"
HOST_ARCHITECTURE="$WINDSHARE_GO_HOST_ARCH"

SECONDS=0
echo "== workflow-lint =="
echo "-- actionlint v1.7.12 (all repository workflows)"

# go run must build for the host even when a caller has selected another GOOS.
# Empty external-linter commands keep the contract identical on every platform.
GOOS="$HOST_OPERATING_SYSTEM" GOARCH="$HOST_ARCHITECTURE" \
    windshare_go run "$ACTIONLINT" -shellcheck= -pyflakes=

echo "== workflow-lint: PASS in ${SECONDS}s =="
