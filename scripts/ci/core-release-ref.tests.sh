#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd -P)"
resolver="$script_dir/core-release-ref.sh"
temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/windshare-core-ref.XXXXXXXX")"
repository="$temporary_root/repository"
remote_repository="$temporary_root/remote.git"
output="$temporary_root/github-output"
stdout_log="$temporary_root/stdout"
stderr_log="$temporary_root/stderr"

# Isolating Git policy keeps fixture refs deterministic on developer machines
# that sign tags or rewrite pushes by default.
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

run_push_resolution() {
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
      bash "$resolver" resolve
  )
}

run_forced_push_resolution() {
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
      bash "$resolver" resolve
  )
}

run_manual_resolution() {
  local version="$1"
  local candidate_sha="$2"
  (
    cd "$repository"
    EVENT_NAME=workflow_dispatch \
      INPUT_COMMIT_SHA="$candidate_sha" \
      INPUT_VERSION="$version" \
      GITHUB_OUTPUT="$output" \
      bash "$resolver" resolve
  )
}

run_publish() {
  local version="$1"
  local candidate_sha="$2"
  (
    cd "$repository"
    CORE_RELEASE_VERSION="$version" \
      CORE_RELEASE_COMMIT_SHA="$candidate_sha" \
      CORE_RELEASE_REMOTE=origin \
      bash "$resolver" publish
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
  [ ! -s "$output" ] || fail "$label emitted candidate outputs before rejection"
}

git init -q -b main "$repository"
git init -q --bare "$remote_repository"
git -C "$repository" config user.name "WindShare release test"
git -C "$repository" config user.email "release-test@invalid.example"
git -C "$repository" remote add origin "$remote_repository"
printf 'candidate\n' >"$repository/fixture.txt"
git -C "$repository" add fixture.txt
git -C "$repository" commit -q -m "candidate"
commit_sha="$(git -C "$repository" rev-parse HEAD)"
moved_sha="$(printf 'conflicting release fixture\n' | git -C "$repository" commit-tree \
  "$(git -C "$repository" rev-parse 'HEAD^{tree}')" -p "$commit_sha")"

candidate_version=v0.3.0
candidate_ref="refs/tags/core-candidate/$candidate_version/candidate-1"
git -C "$repository" tag "${candidate_ref#refs/tags/}" "$commit_sha"
: >"$output"
run_push_resolution "$candidate_ref" "$commit_sha"
grep -Fxq "commit_sha=$commit_sha" "$output" || fail "candidate output omitted the exact commit"
grep -Fxq "version=$candidate_version" "$output" || fail "candidate output omitted the exact version"
expect_failure "forced candidate update" "accepts only a newly created, non-forced tag" \
  run_forced_push_resolution "$candidate_ref" "$commit_sha"

git -C "$repository" tag -d "${candidate_ref#refs/tags/}" >/dev/null
git -C "$repository" tag -a -m "annotated candidate" "${candidate_ref#refs/tags/}" "$commit_sha"
expect_failure "annotated candidate" "candidate tag must directly reference a commit" \
  run_push_resolution "$candidate_ref" "$commit_sha"
git -C "$repository" tag -d "${candidate_ref#refs/tags/}" >/dev/null
git -C "$repository" tag "${candidate_ref#refs/tags/}" "$commit_sha"

missing_candidate_ref="refs/tags/core-candidate/$candidate_version/"
expect_failure "missing candidate name" "candidate tag must have a non-empty name" \
  run_push_resolution "$missing_candidate_ref" "$commit_sha"

nested_candidate_ref="refs/tags/core-candidate/$candidate_version/nightly/candidate-2"
git -C "$repository" tag "${nested_candidate_ref#refs/tags/}" "$commit_sha"
: >"$output"
run_push_resolution "$nested_candidate_ref" "$commit_sha"
grep -Fxq "commit_sha=$commit_sha" "$output" || fail "nested candidate omitted the exact commit"
grep -Fxq "version=$candidate_version" "$output" || fail "nested candidate omitted the exact version"

invalid_version=v01.2.3
invalid_ref="refs/tags/core-candidate/$invalid_version/candidate-1"
git -C "$repository" tag "${invalid_ref#refs/tags/}" "$commit_sha"
expect_failure "non-canonical candidate version" "candidate version must have the form vX.Y.Z" \
  run_push_resolution "$invalid_ref" "$commit_sha"

: >"$output"
run_manual_resolution "$candidate_version" "$commit_sha"
grep -Fxq "commit_sha=$commit_sha" "$output" || fail "manual output omitted the exact commit"
grep -Fxq "version=$candidate_version" "$output" || fail "manual output omitted the exact version"
expect_failure "manual prerelease version" "candidate version must have the form vX.Y.Z" \
  run_manual_resolution v1.2.3-rc.1 "$commit_sha"
expect_failure "missing manual commit" "manual candidate is not an available Git object" \
  run_manual_resolution v1.2.3 1111111111111111111111111111111111111111

published_version=v1.2.3
published_ref="refs/tags/core/$published_version"
run_publish "$published_version" "$commit_sha"
published_sha="$(git --git-dir="$remote_repository" rev-parse "$published_ref")"
[ "$published_sha" = "$commit_sha" ] || fail "missing release was not created at the candidate commit"
run_publish "$published_version" "$commit_sha"

annotated_version=v1.2.4
annotated_ref="refs/tags/core/$annotated_version"
git -C "$repository" tag -a -m "existing annotated release" \
  "${annotated_ref#refs/tags/}" "$commit_sha"
git -C "$repository" push -q origin "$annotated_ref:$annotated_ref"
run_publish "$annotated_version" "$commit_sha"

conflict_version=v1.2.5
conflict_ref="refs/tags/core/$conflict_version"
git -C "$repository" push -q origin "$moved_sha:$conflict_ref"
expect_failure "conflicting release" "refusing to move it to $commit_sha" \
  run_publish "$conflict_version" "$commit_sha"
conflict_sha="$(git --git-dir="$remote_repository" rev-parse "$conflict_ref")"
[ "$conflict_sha" = "$moved_sha" ] || fail "conflicting release ref moved after rejection"

trap - EXIT
rm -rf -- "$temporary_root"
echo "core release ref tests: PASS"
