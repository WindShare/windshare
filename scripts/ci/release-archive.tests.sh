#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

repository_root="$(pwd -P)"
readonly artifact_version=v0.0.0-ci
test_parent="${TMPDIR:-/tmp}"
test_parent="${test_parent%/}"
if [ -z "$test_parent" ]; then
  test_parent="/"
fi
test_root="$(mktemp -d "$test_parent/windshare-release-archive.XXXXXXXX")"
fixture_repository="$test_root/repository"
release_environment="$test_root/release-environment"

cleanup() {
  local status="$?"
  trap - EXIT
  case "$test_root" in
    "$test_parent"/windshare-release-archive.*) rm -rf -- "$test_root" ;;
    *) echo "refusing to remove unowned archive-test path: $test_root" >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT

fail() {
  echo "release committed archive contract: $1" >&2
  exit 1
}

export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_NO_REPLACE_OBJECTS=1
export GIT_TERMINAL_PROMPT=0
git clone --quiet --no-hardlinks -- "$repository_root" "$fixture_repository"
git -C "$fixture_repository" rm --quiet --ignore-unmatch core/go.mod core/go.sum go.work go.work.sum
git -C "$fixture_repository" -c user.name=WindShare -c user.email=release-contract.invalid \
  commit --quiet --allow-empty -m single-module-fixture
commit_sha="$(git -C "$fixture_repository" rev-parse HEAD)"
[[ "$commit_sha" =~ ^[0-9a-f]{40}$ ]] || fail "fixture HEAD is not an exact commit SHA"
[ -z "$(git -C "$fixture_repository" status --porcelain=v1 --untracked-files=all)" ] ||
  fail "fixture checkout was not initially clean"
expected_readme_object="$(git -C "$fixture_repository" rev-parse "$commit_sha:core/README.md")"

printf 'tracked mutation after clean proof\n' >"$fixture_repository/core/README.md"
printf 'untracked\n' >"$fixture_repository/untracked-after-clean.txt"
git -C "$fixture_repository" add -- core/README.md

install -d -m 0700 -- "$release_environment"
source scripts/ci/release-environment.sh
windshare_prepare_release_go_environment "$release_environment"
go run ./scripts/ci/_sourcebundle \
  -repo "$fixture_repository" \
  -commit "$commit_sha" \
  -stage "$test_root/committed-module" \
  -zip "$test_root/source.zip" \
  -extract "$test_root/extracted-module" \
  -version "$artifact_version" >/dev/null

actual_readme_object="$(git hash-object -- "$test_root/extracted-module/core/README.md")"
[ "$actual_readme_object" = "$expected_readme_object" ] ||
  fail "archive consumed tracked worktree/index bytes"
[ ! -e "$test_root/extracted-module/untracked-after-clean.txt" ] ||
  fail "archive consumed an untracked worktree file"

trap - EXIT
rm -rf -- "$test_root"
echo "release committed archive contract: PASS"
