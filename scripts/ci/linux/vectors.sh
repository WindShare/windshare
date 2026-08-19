#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== vectors =="

echo "-- verify protocol-contract vectors"
go test -count=1 ./core/internal/protocolcontract
echo "-- verify peer-signaling vectors"
go test -count=1 ./connectivity/v2signal
echo "-- verify diagnostic-correlation vectors"
go test -count=1 ./cmd/wind/internal/runtrace -run 'Test(CorrelationV1|DiagnosticCorrelationVectors)'

echo "== vectors: PASS in ${SECONDS}s =="
