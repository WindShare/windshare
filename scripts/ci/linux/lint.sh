#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== lint =="

echo "-- golangci-lint (root)"
golangci-lint run ./...

echo "-- golangci-lint (core)"
(
  cd core
  golangci-lint run ./...
)

echo "== lint: PASS in ${SECONDS}s =="
