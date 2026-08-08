#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== vet =="

echo "-- go vet (root)"
go vet ./...

echo "-- go vet (core)"
go -C core vet ./...

echo "== vet: PASS in ${SECONDS}s =="
