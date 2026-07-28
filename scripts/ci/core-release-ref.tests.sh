#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd -P)"
resolver="$script_dir/core-release-ref.sh"
temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/windshare-core-ref.XXXXXXXX")"
repository="$temporary_root/repository"
output="$temporary_root/github-output"
stdout_log="$temporary_root/stdout"
stderr_log="$temporary_root/stderr"

# Host Git policy must not silently sign a fixture tag or rewrite its bytes;
# the tests are proving raw object type, so their repository is self-contained.
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_TERMINAL_PROMPT=0

cleanup() {
  local status="$?"
  trap - EXIT
  rm -rf -- "$temporary_root"
  exit "$status"
}
trap cleanup EXIT

fail() {
  echo "core release ref tests: $1" >&2
  exit 1
}

run_push() {
  local event_ref="$1"
  local event_sha="$2"
  (
    cd "$repository"
    EVENT_NAME=push \
      EVENT_CREATED=true \
      EVENT_DELETED=false \
      EVENT_FORCED=false \
      EVENT_BEFORE=0000000000000000000000000000000000000000 \
      EVENT_AFTER="$event_sha" \
      EVENT_REF="$event_ref" \
      EVENT_SHA="$event_sha" \
      GITHUB_OUTPUT="$output" \
      bash "$resolver"
  )
}

run_moved_push() {
  local event_ref="$1"
  local event_sha="$2"
  (
    cd "$repository"
    EVENT_NAME=push \
      EVENT_CREATED=false \
      EVENT_DELETED=false \
      EVENT_FORCED=true \
      EVENT_BEFORE="$event_sha" \
      EVENT_AFTER="$event_sha" \
      EVENT_REF="$event_ref" \
      EVENT_SHA="$event_sha" \
      GITHUB_OUTPUT="$output" \
      bash "$resolver"
  )
}

run_manual() {
  local version="$1"
  local candidate_sha="$2"
  (
    cd "$repository"
    EVENT_NAME=workflow_dispatch \
      INPUT_COMMIT_SHA="$candidate_sha" \
      INPUT_VERSION="$version" \
      GITHUB_OUTPUT="$output" \
      bash "$resolver"
  )
}

expect_failure() {
  local label="$1"
  local expected="$2"
  shift 2
  : >"$output"
  : >"$stdout_log"
  : >"$stderr_log"
  if "$@" >"$stdout_log" 2>"$stderr_log"; then
    fail "$label did not fail closed"
  fi
  grep -Fq -- "$expected" "$stderr_log" ||
    fail "$label did not report the expected reason: $expected"
  [ ! -s "$output" ] || fail "$label emitted release outputs before rejection"
}

expect_tag_version_rejected() {
  local label="$1"
  local version="$2"
  local expected="$3"
  local candidate="refs/tags/core-candidate/$version/$commit_sha"
  local final="refs/tags/core/$version"

  git -C "$repository" tag "${candidate#refs/tags/}" "$commit_sha"
  expect_failure "$label candidate tag" "$expected" run_push "$candidate" "$commit_sha"
  git -C "$repository" tag -d "${candidate#refs/tags/}" >/dev/null

  git -C "$repository" tag "${final#refs/tags/}" "$commit_sha"
  expect_failure "$label final tag" "$expected" run_push "$final" "$commit_sha"
  git -C "$repository" tag -d "${final#refs/tags/}" >/dev/null
}

git init -q -b main "$repository"
git -C "$repository" config user.name "WindShare release test"
git -C "$repository" config user.email "release-test@invalid.example"
printf 'candidate\n' >"$repository/fixture.txt"
git -C "$repository" add fixture.txt
git -C "$repository" commit -q -m "candidate"
commit_sha="$(git -C "$repository" rev-parse HEAD)"
moved_sha="$(printf 'moved ref fixture\n' | git -C "$repository" commit-tree \
  "$(git -C "$repository" rev-parse 'HEAD^{tree}')" -p "$commit_sha")"
test_version=v0.0.0-release-test
artifact_only_version=v0.0.0-ci
closed_release_version=v0.3.0
candidate_ref="refs/tags/core-candidate/$test_version/$commit_sha"
final_ref="refs/tags/core/$test_version"

git -C "$repository" tag "${candidate_ref#refs/tags/}" "$commit_sha"
: >"$output"
run_push "$candidate_ref" "$commit_sha"
grep -Fxq "commit_sha=$commit_sha" "$output" || fail "candidate output omitted the exact commit"
grep -Fxq "version=$test_version" "$output" || fail "candidate output omitted the exact version"

git -C "$repository" tag -d "${candidate_ref#refs/tags/}" >/dev/null
git -C "$repository" tag -a -m "annotated candidate" "${candidate_ref#refs/tags/}" "$commit_sha"
expect_failure "annotated candidate" "event tag must directly reference a commit" \
  run_push "$candidate_ref" "$commit_sha"

git -C "$repository" tag -d "${candidate_ref#refs/tags/}" >/dev/null
git -C "$repository" tag "${candidate_ref#refs/tags/}" "$commit_sha"
git -C "$repository" tag "${final_ref#refs/tags/}" "$commit_sha"
: >"$output"
run_push "$final_ref" "$commit_sha"
grep -Fxq "commit_sha=$commit_sha" "$output" || fail "final output omitted the exact commit"
expect_failure "moved final tag" "accepts only a newly created, non-forced tag" \
  run_moved_push "$final_ref" "$commit_sha"
git -C "$repository" tag -f "${final_ref#refs/tags/}" "$moved_sha" >/dev/null
expect_failure "final ref moved before resolution" "event tag does not equal the expected commit" \
  run_push "$final_ref" "$commit_sha"

git -C "$repository" tag -d "${final_ref#refs/tags/}" >/dev/null
git -C "$repository" tag -a -m "annotated final" "${final_ref#refs/tags/}" "$commit_sha"
expect_failure "annotated final" "event tag must directly reference a commit" \
  run_push "$final_ref" "$commit_sha"

git -C "$repository" tag -d "${final_ref#refs/tags/}" >/dev/null
git -C "$repository" tag "${final_ref#refs/tags/}" "$commit_sha"
git -C "$repository" tag -d "${candidate_ref#refs/tags/}" >/dev/null
git -C "$repository" tag -a -m "annotated candidate" "${candidate_ref#refs/tags/}" "$commit_sha"
expect_failure "annotated matching candidate" "matching candidate tag must directly reference a commit" \
  run_push "$final_ref" "$commit_sha"

git -C "$repository" tag -d "${candidate_ref#refs/tags/}" >/dev/null
git -C "$repository" tag "${candidate_ref#refs/tags/}" "$moved_sha"
expect_failure "matching candidate moved before final resolution" \
  "matching candidate tag does not equal the expected commit" \
  run_push "$final_ref" "$commit_sha"

git -C "$repository" tag -d "${candidate_ref#refs/tags/}" >/dev/null
wrong_suffix=0000000000000000000000000000000000000000
wrong_ref="refs/tags/core-candidate/$test_version/$wrong_suffix"
git -C "$repository" tag "${wrong_ref#refs/tags/}" "$commit_sha"
expect_failure "candidate suffix mismatch" "candidate tag SHA suffix does not match github.sha" \
  run_push "$wrong_ref" "$commit_sha"

expect_tag_version_rejected \
  "closed version" \
  "$closed_release_version" \
  "release version is closed and cannot be verified again: $closed_release_version"
expect_tag_version_rejected \
  "artifact-only version" \
  "$artifact_only_version" \
  "release version is reserved for non-publishing artifact checks: $artifact_only_version"

: >"$output"
run_manual "$test_version" "$commit_sha"
grep -Fxq "commit_sha=$commit_sha" "$output" || fail "manual output omitted the exact commit"
grep -Fxq "version=$test_version" "$output" || fail "manual output omitted the exact version"

expect_failure \
  "closed manual version" \
  "release version is closed and cannot be verified again: $closed_release_version" \
  run_manual "$closed_release_version" "$commit_sha"
expect_failure \
  "artifact-only manual version" \
  "release version is reserved for non-publishing artifact checks: $artifact_only_version" \
  run_manual "$artifact_only_version" "$commit_sha"
expect_failure \
  "missing manual version" \
  "candidate version is not a safe semantic version" \
  run_manual "" "$commit_sha"
expect_failure \
  "missing manual commit" \
  "manual candidate is not an available commit object" \
  run_manual "$test_version" 1111111111111111111111111111111111111111

trap - EXIT
rm -rf -- "$temporary_root"
echo "core release ref tests: PASS"
