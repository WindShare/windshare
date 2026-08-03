#!/usr/bin/env bash
# GitHub workflow lint gate for the native Linux toolchain.
set -euo pipefail
cd "$(dirname "$0")/../../.."

ACTIONLINT='github.com/rhysd/actionlint/cmd/actionlint@v1.7.12'

SECONDS=0
echo "== workflow-lint =="

host_operating_system="$(go env GOHOSTOS)"
host_architecture="$(go env GOHOSTARCH)"
if [[ "$host_operating_system" != linux ]]; then
  echo "Linux workflow lint requires a Linux Go host, received: $host_operating_system" >&2
  exit 1
fi

# go run must build for the host even if the caller selected a target GOOS.
# Empty external-linter commands keep this gate independent of host packages.
GOOS="$host_operating_system" GOARCH="$host_architecture" \
  go run "$ACTIONLINT" -shellcheck= -pyflakes=

echo "== workflow-lint: PASS in ${SECONDS}s =="
