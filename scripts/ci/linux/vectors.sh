#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== vectors =="

echo "-- verify protocol-contract vectors"
go -C core test -count=1 ./internal/protocolcontract
echo "-- verify peer-signaling vectors"
go test -count=1 ./connectivity/v2signal

echo "== vectors: PASS in ${SECONDS}s =="
