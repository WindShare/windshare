#!/usr/bin/env bash
# CI-parity race gate (Linux). Mirrors the -race test steps of ci.yml go-root
# and go-core verbatim. On Linux the OS-network constructors
# (internal/testnetwork) are open, so these sweeps run the real-socket cases
# ungated — exactly as the ubuntu jobs do.
set -euo pipefail
cd "$(dirname "$0")/../.."

SECONDS=0
core_suite_test_timeout='30m'
echo "== race =="

echo "-- go test -race (root, OS-network cases ungated)"
go test -race -count=1 ./...

echo "-- go test -race (core)"
go -C core test -race -count=1 -timeout="$core_suite_test_timeout" ./...

echo "== race: PASS in ${SECONDS}s =="
