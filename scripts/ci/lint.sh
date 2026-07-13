#!/usr/bin/env bash
# CI-parity lint gate (Linux). Mirrors ci.yml job `lint` verbatim:
# golangci-lint v2 over both Go modules (root, then core), governed by the
# one root .golangci.yml each run discovers upward. The tool is
# version-pinned and executed via `go run` — the GO_TEST_COVERAGE precedent —
# so local and CI always run the identical linter version with no PATH
# prerequisite; the Go build cache absorbs the one-time source build.
set -euo pipefail
cd "$(dirname "$0")/../.."

# Same pin as ci.yml env GOLANGCI_LINT and scripts/ci/lint.ps1; bump together.
GOLANGCI_LINT='github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2'

SECONDS=0
echo "== lint =="

echo "-- golangci-lint (root)"
go run "$GOLANGCI_LINT" run ./...

echo "-- golangci-lint (core)"
go -C core run "$GOLANGCI_LINT" run ./...

echo "== lint: PASS in ${SECONDS}s =="
