#!/usr/bin/env bash
# Coverage is one ordinary sweep per Go module with -covermode=atomic, followed
# by the pinned go-test-coverage v2.18.8 verdicts driven
# by each module's .testcoverage.yml (core total >=90%, root total >=80%,
# every package >=70%). Profiles stage in a mktemp dir instead of CI's
# in-workdir cover.out so the gate never dirties the worktree.
# The root sweep owns integration/E2E, so one gate run ID spans their packages.
# JSON mode keeps passing scenario evidence visible in that single instrumented
# sweep instead of rerunning the packages without coverage.
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/goauthority/authority.sh
windshare_enter_go_authority
source scripts/ci/test-run-id.sh

# Same pinned gate tool as ci.yml (env GO_TEST_COVERAGE).
GO_TEST_COVERAGE='github.com/vladopajic/go-test-coverage/v2@v2.18.8'
core_suite_test_timeout='30m'

SECONDS=0
generated_run_id="$(new_windshare_test_run_id coverage)"
export WINDSHARE_TEST_RUN_ID="$generated_run_id"
echo "== coverage: run_id=$WINDSHARE_TEST_RUN_ID =="

profile_dir="$(mktemp -d)"
trap 'rm -rf "$profile_dir"' EXIT

echo "-- root module coverage tests"
windshare_go_test_json -count=1 ./... -covermode=atomic -coverprofile="$profile_dir/root.cover.out"
echo "-- root coverage gate (total >=80%, package >=70%)"
windshare_go run "$GO_TEST_COVERAGE" --config=.testcoverage.yml --profile="$profile_dir/root.cover.out"

echo "-- core module coverage tests"
windshare_go -C core test -count=1 -timeout="$core_suite_test_timeout" ./... \
  -covermode=atomic -coverprofile="$profile_dir/core.cover.out"
echo "-- core coverage gate (total >=90%, package >=70%)"
(cd core && windshare_go run "$GO_TEST_COVERAGE" --config=.testcoverage.yml --profile="$profile_dir/core.cover.out")

echo "== coverage: PASS in ${SECONDS}s =="
