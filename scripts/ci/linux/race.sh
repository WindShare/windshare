#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== race =="

echo "-- root short race sweep"
go test -short -race -count=1 ./...

echo "-- core short race sweep"
go -C core test -short -race -count=1 ./...

echo "== race: PASS in ${SECONDS}s =="
