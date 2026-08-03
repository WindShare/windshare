#!/usr/bin/env bash
# Root already owns its integration and E2E packages, so a single module sweep
# preserves complete race coverage without replaying selected workloads. One
# gate-owned run ID keeps every package and child process in that sweep joinable.
# The root sweep stays JSON-visible so race instrumentation cannot hide passing
# integration/E2E scenario events.
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/test-run-id.sh

SECONDS=0
core_suite_test_timeout='30m'
generated_run_id="$(new_windshare_test_run_id race)"
export WINDSHARE_TEST_RUN_ID="$generated_run_id"
echo "== race: run_id=$WINDSHARE_TEST_RUN_ID =="

echo "-- go test -race (root)"
go test -json -race -count=1 ./...

echo "-- go test -race (core)"
go -C core test -race -count=1 -timeout="$core_suite_test_timeout" ./...

echo "== race: PASS in ${SECONDS}s =="
