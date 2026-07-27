#!/usr/bin/env bash
# CI-parity coverage gate (Linux). Mirrors the coverage test + gate steps of
# ci.yml go-root and go-core verbatim: full ungated sweeps with
# -covermode=atomic, then the pinned go-test-coverage v2.18.8 verdicts driven
# by each module's .testcoverage.yml (core total >=90%, root total >=80%,
# every package >=70%). Profiles stage in a mktemp dir instead of CI's
# in-workdir cover.out so the gate never dirties the worktree.
set -euo pipefail
cd "$(dirname "$0")/../.."

# Same pinned gate tool as ci.yml (env GO_TEST_COVERAGE).
GO_TEST_COVERAGE='github.com/vladopajic/go-test-coverage/v2@v2.18.8'
core_suite_test_timeout='30m'

SECONDS=0
echo "== coverage =="

profile_dir="$(mktemp -d)"
trap 'rm -rf "$profile_dir"' EXIT

echo "-- root module coverage tests"
go test -count=1 ./... -covermode=atomic -coverprofile="$profile_dir/root.cover.out"
echo "-- root coverage gate (total >=80%, package >=70%)"
go run "$GO_TEST_COVERAGE" --config=.testcoverage.yml --profile="$profile_dir/root.cover.out"

echo "-- core module coverage tests"
go -C core test -count=1 -timeout="$core_suite_test_timeout" ./... \
  -covermode=atomic -coverprofile="$profile_dir/core.cover.out"
echo "-- core coverage gate (total >=90%, package >=70%)"
(cd core && go run "$GO_TEST_COVERAGE" --config=.testcoverage.yml --profile="$profile_dir/core.cover.out")

echo "== coverage: PASS in ${SECONDS}s =="
