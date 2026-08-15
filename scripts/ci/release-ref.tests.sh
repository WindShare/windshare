#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
script_path="$(pwd -P)/scripts/ci/release-ref.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/windshare-release-ref.XXXXXXXX")"
cleanup() {
  rm -rf -- "$test_root"
}
trap cleanup EXIT

remote="$test_root/remote.git"
repository="$test_root/repository"
git init --quiet --bare "$remote"
git init --quiet "$repository"
git -C "$repository" config user.name "Release Ref Tests"
git -C "$repository" config user.email "release-ref@example.invalid"
git -C "$repository" branch -M main
printf 'main\n' >"$repository/value.txt"
git -C "$repository" add value.txt
git -C "$repository" commit --quiet -m main
main_sha="$(git -C "$repository" rev-parse HEAD)"
git -C "$repository" remote add origin "$remote"
git -C "$repository" push --quiet -u origin main

git -C "$repository" switch --quiet -c side
printf 'side\n' >"$repository/value.txt"
git -C "$repository" commit --quiet -am side
side_sha="$(git -C "$repository" rev-parse HEAD)"
git -C "$repository" switch --quiet main

expect_failure() {
  local expected="$1"
  shift
  local output
  set +e
  output="$("$@" 2>&1)"
  local status="$?"
  set -e
  if [ "$status" -eq 0 ]; then
    echo "expected command to fail: $*" >&2
    exit 1
  fi
  if [[ "$output" != *"$expected"* ]]; then
    echo "failure did not contain '$expected': $output" >&2
    exit 1
  fi
}

resolve_output="$test_root/resolve.out"
(
  cd "$repository"
  GITHUB_OUTPUT="$resolve_output" \
  INPUT_COMMIT_SHA="$main_sha" \
  INPUT_VERSION=v1.2.3 \
  DEFAULT_BRANCH=main \
    bash "$script_path" resolve
)
grep -Fxq "commit_sha=$main_sha" "$resolve_output"
grep -Fxq "version=v1.2.3" "$resolve_output"

expect_failure "reachable from origin/main" \
  env GITHUB_OUTPUT="$test_root/side.out" \
      INPUT_COMMIT_SHA="$side_sha" \
      INPUT_VERSION=v1.2.3 \
      DEFAULT_BRANCH=main \
      bash -c 'cd "$1" && bash "$2" resolve' _ "$repository" "$script_path"
expect_failure "exact lowercase 40-character SHA" \
  env GITHUB_OUTPUT="$test_root/bad-sha.out" \
      INPUT_COMMIT_SHA=HEAD \
      INPUT_VERSION=v1.2.3 \
      DEFAULT_BRANCH=main \
      bash -c 'cd "$1" && bash "$2" resolve' _ "$repository" "$script_path"
expect_failure "form vX.Y.Z" \
  env GITHUB_OUTPUT="$test_root/bad-version.out" \
      INPUT_COMMIT_SHA="$main_sha" \
      INPUT_VERSION=v01.2.3 \
      DEFAULT_BRANCH=main \
      bash -c 'cd "$1" && bash "$2" resolve' _ "$repository" "$script_path"

(
  cd "$repository"
  RELEASE_VERSION=v1.2.3 \
  RELEASE_COMMIT_SHA="$main_sha" \
  DEFAULT_BRANCH=main \
    bash "$script_path" publish
)
[ "$(git --git-dir="$remote" rev-parse refs/tags/v1.2.3)" = "$main_sha" ]

# Publishing the same immutable result is idempotent.
(
  cd "$repository"
  RELEASE_VERSION=v1.2.3 \
  RELEASE_COMMIT_SHA="$main_sha" \
  DEFAULT_BRANCH=main \
    bash "$script_path" publish
)

git -C "$repository" push --quiet origin "$side_sha:refs/tags/v1.2.4"
expect_failure "refusing to validate a move" \
  env GITHUB_OUTPUT="$test_root/divergent.out" \
      INPUT_COMMIT_SHA="$main_sha" \
      INPUT_VERSION=v1.2.4 \
      DEFAULT_BRANCH=main \
      bash -c 'cd "$1" && bash "$2" resolve' _ "$repository" "$script_path"
expect_failure "refusing to move it" \
  env RELEASE_VERSION=v1.2.4 \
      RELEASE_COMMIT_SHA="$main_sha" \
      DEFAULT_BRANCH=main \
      bash -c 'cd "$1" && bash "$2" publish' _ "$repository" "$script_path"
[ "$(git --git-dir="$remote" rev-parse refs/tags/v1.2.4)" = "$side_sha" ]

if grep -Eq 'core-candidate|refs/tags/core/' "$script_path"; then
  echo "release ref helper retained a core candidate/tag namespace" >&2
  exit 1
fi

echo "release ref tests: PASS"
