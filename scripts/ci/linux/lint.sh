#!/usr/bin/env bash
# Cross-platform lint gate (Linux host). The pinned linter is installed for the
# host before GOOS is varied: `go run` honors GOOS and would otherwise build a
# target executable that the host cannot launch. Each module is then analyzed
# for both production OS file sets with the same root configuration.
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority

# Same pin as scripts/ci/windows/lint.ps1; bump both platform entry points together.
GOLANGCI_LINT='github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2'
TARGET_OPERATING_SYSTEMS=(linux windows)

SECONDS=0
echo "== lint =="

bash scripts/ci/linux/workflow-lint.sh

HOST_OPERATING_SYSTEM="$WINDSHARE_GO_HOST_OS"
HOST_ARCHITECTURE="$WINDSHARE_GO_HOST_ARCH"
GO_BIN="$(windshare_go env GOBIN)"
if [[ -z "$GO_BIN" ]]; then
    # Go installs commands into the first GOPATH entry when GOBIN is unset.
    GO_PATH="$(windshare_go env GOPATH)"
    GO_BIN="${GO_PATH%%:*}/bin"
fi
LINTER_PATH="$GO_BIN/golangci-lint"

# Installing under host settings keeps the executable runnable while the later
# invocations select target-specific source files through GOOS.
echo "-- install golangci-lint (host)"
GOOS="$HOST_OPERATING_SYSTEM" GOARCH="$HOST_ARCHITECTURE" windshare_go install "$GOLANGCI_LINT"
if [[ ! -x "$LINTER_PATH" ]]; then
    echo "golangci-lint was not installed at $LINTER_PATH" >&2
    exit 1
fi

for target_operating_system in "${TARGET_OPERATING_SYSTEMS[@]}"; do
    echo "-- golangci-lint (root, GOOS=$target_operating_system)"
    GOOS="$target_operating_system" GOARCH="$HOST_ARCHITECTURE" \
        "$LINTER_PATH" run ./...

    echo "-- golangci-lint (core, GOOS=$target_operating_system)"
    (
        cd core
        GOOS="$target_operating_system" GOARCH="$HOST_ARCHITECTURE" \
            "$LINTER_PATH" run ./...
    )
done

echo "== lint: PASS in ${SECONDS}s =="
