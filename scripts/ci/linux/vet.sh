#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== vet =="

echo "-- go vet (root)"
go vet ./...

echo "-- go vet (core)"
go -C core vet ./...

# This is the one build that tests a different dependency graph: disabling the
# workspace proves the root module consumes the released core module cleanly.
echo "-- GOWORK=off released-core consumer build"
GOWORK=off go build ./...

echo "== vet: PASS in ${SECONDS}s =="
