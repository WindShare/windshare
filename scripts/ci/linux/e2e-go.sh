#!/usr/bin/env bash
# E2E scenarios publish their verdicts to stdout, so the shared JSON boundary is
# required even on success; a second verbose replay would create false evidence.
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/test-run-id.sh

SECONDS=0
generated_run_id="$(new_windshare_test_run_id e2e-go)"
export WINDSHARE_TEST_RUN_ID="$generated_run_id"
echo "== e2e-go: run_id=$WINDSHARE_TEST_RUN_ID =="
go test -json -count=1 ./e2e
echo "== e2e-go: PASS in ${SECONDS}s =="
