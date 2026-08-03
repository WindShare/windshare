#!/usr/bin/env bash
# Linux owns the repository's single Go lint sweep. The linter itself is built
# for the native host, then GOOS selects both production file sets for analysis.
set -euo pipefail
cd "$(dirname "$0")/../../.."

# Keep this aligned with the Windows developer entry point while it remains.
GOLANGCI_LINT='github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2'
TARGET_OPERATING_SYSTEMS=(linux windows)

SECONDS=0
echo "== lint =="

HOST_OPERATING_SYSTEM="$(go env GOHOSTOS)"
HOST_ARCHITECTURE="$(go env GOHOSTARCH)"
if [[ "$HOST_OPERATING_SYSTEM" != linux ]]; then
    echo "Linux lint requires a Linux Go host, received: $HOST_OPERATING_SYSTEM" >&2
    exit 1
fi

GO_BIN="$(go env GOBIN)"
if [[ -z "$GO_BIN" ]]; then
    # Go installs commands into the first GOPATH entry when GOBIN is unset.
    GO_PATH="$(go env GOPATH)"
    GO_BIN="${GO_PATH%%:*}/bin"
fi
LINTER_PATH="$GO_BIN/golangci-lint"

# Installing under host settings keeps the executable runnable while the later
# invocations select target-specific source files through GOOS.
echo "-- install golangci-lint (host)"
GOOS="$HOST_OPERATING_SYSTEM" GOARCH="$HOST_ARCHITECTURE" go install "$GOLANGCI_LINT"
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
