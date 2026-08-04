#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== e2e =="
go test ./e2e -run '^TestCritical' -count=1
echo "== e2e: PASS in ${SECONDS}s =="
