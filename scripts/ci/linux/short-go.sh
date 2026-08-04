#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."

SECONDS=0
echo "== short-go =="

profile_dir="$(mktemp -d)"
root_profile="$profile_dir/root.cover.out"
core_profile="$profile_dir/core.cover.out"
cleanup() {
  rm -f -- "$root_profile" "$core_profile"
  rmdir -- "$profile_dir"
}
trap cleanup EXIT

echo "-- root short race and atomic coverage sweep"
go test -short -race -count=1 -covermode=atomic -coverprofile="$root_profile" ./...

echo "-- root coverage verdict"
go-test-coverage --config=.testcoverage.yml --profile="$root_profile"

echo "-- core short race and atomic coverage sweep"
go -C core test -short -race -count=1 -covermode=atomic -coverprofile="$core_profile" ./...

echo "-- core coverage verdict"
(
  cd core
  go-test-coverage --config=.testcoverage.yml --profile="$core_profile"
)

echo "== short-go: PASS in ${SECONDS}s =="
