#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== vectors-update =="

echo "-- update protocol-contract vectors"
go test -count=1 ./core/internal/protocolcontract -update
echo "-- update peer-signaling vectors"
go test -count=1 ./connectivity/v2signal -update

echo "== vectors-update: PASS in ${SECONDS}s =="
