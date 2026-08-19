#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
source scripts/ci/linux/go-package-sets.sh
windshare_load_go_package_set non-core
non_core_packages=("${WINDSHARE_GO_PACKAGES[@]}")
windshare_load_go_package_set core
core_packages=("${WINDSHARE_GO_PACKAGES[@]}")

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

# Hosted CI owns forced reruns; local validation keeps Go's content-aware cache
# so unchanged race/coverage packages do not dominate feedback time.
echo "-- non-core short race and atomic coverage"
go test -short -race -covermode=atomic -coverprofile="$root_profile" "${non_core_packages[@]}"

echo "-- non-core coverage verdict"
go-test-coverage --config=.testcoverage.yml --profile="$root_profile"

echo "-- core short race and atomic coverage"
go test -short -race -covermode=atomic -coverprofile="$core_profile" "${core_packages[@]}"

echo "-- core coverage verdict"
go-test-coverage --config=core/.testcoverage.yml --profile="$core_profile" --source-dir=core

echo "== short-go: PASS in ${SECONDS}s =="
