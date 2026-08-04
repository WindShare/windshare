#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== check =="

echo "-- root short tests"
go test -short ./...

echo "-- core short tests"
go -C core test -short ./...

echo "-- Web typecheck"
pnpm -C web exec tsc -b --force

echo "-- Web unit tests"
pnpm -C web test

echo "== check: PASS in ${SECONDS}s =="
