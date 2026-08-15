#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== long-go =="

echo "-- named E2E long suites"
go test -count=1 -run '^TestLong' ./e2e

echo "-- integration packages"
go test -count=1 ./integration/relayv2 ./integration/v2peer

echo "-- catalog long suites"
go test -count=1 -run '^TestLong' ./core/catalog

echo "-- output runtime long suites"
go test -count=1 -run '^TestLong' ./core/osfs/internal/outputruntime

echo "== long-go: PASS in ${SECONDS}s =="
