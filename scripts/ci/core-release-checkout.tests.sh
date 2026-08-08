#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

source scripts/ci/core-release-checkout.sh

test_parent="${TMPDIR:-/tmp}"
test_parent="${test_parent%/}"
if [ -z "$test_parent" ]; then
  test_parent="/"
fi
test_root="$(mktemp -d "$test_parent/windshare-core-checkout.XXXXXXXX")"
fixture_repository="$test_root/repository"
linked_worktree="$test_root/linked-worktree"
exact_checkout="$test_root/exact-checkout"

cleanup() {
  local status="$?"
  trap - EXIT
  case "$test_root" in
    "$test_parent"/windshare-core-checkout.*) rm -rf -- "$test_root" ;;
    *) echo "refusing to remove unowned checkout-test path: $test_root" >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT

fail() {
  echo "core release checkout contract: $1" >&2
  exit 1
}

expect_checkout_failure() {
  local repository_root="$1"
  local label="$2"

  if windshare_assert_exact_release_checkout "$repository_root" "$commit_sha" \
    >"$test_root/failure.log" 2>&1; then
    fail "$label did not fail closed"
  fi
}

mkdir -- "$fixture_repository"
windshare_run_isolated_git -C "$fixture_repository" init --quiet
printf 'committed release input\n' >"$fixture_repository/tracked.txt"
windshare_run_isolated_git -C "$fixture_repository" add -- tracked.txt
windshare_run_isolated_git -C "$fixture_repository" \
  -c user.name=WindShare -c user.email=release-contract.invalid \
  commit --quiet -m fixture
commit_sha="$(windshare_run_isolated_git -C "$fixture_repository" rev-parse HEAD)"
[[ "$commit_sha" =~ ^[0-9a-f]{40}$ ]] || fail "fixture commit is not an exact SHA"
windshare_run_isolated_git -C "$fixture_repository" \
  worktree add --quiet --detach "$linked_worktree" "$commit_sha"

# -C does not override these caller redirects. The contract must clear them and
# still validate the explicitly named repository.
export GIT_DIR="$test_root/redirected.git"
export GIT_WORK_TREE="$test_root/redirected-worktree"
export GIT_INDEX_FILE="$test_root/redirected-index"
windshare_assert_exact_release_checkout "$fixture_repository" "$commit_sha"
windshare_assert_exact_release_checkout "$linked_worktree" "$commit_sha"
[ "$GIT_DIR" = "$test_root/redirected.git" ] || fail "GIT_DIR was not restored"
[ "$GIT_WORK_TREE" = "$test_root/redirected-worktree" ] || fail "GIT_WORK_TREE was not restored"
[ "$GIT_INDEX_FILE" = "$test_root/redirected-index" ] || fail "GIT_INDEX_FILE was not restored"
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE

printf 'ordinary tracked mutation\n' >"$fixture_repository/tracked.txt"
expect_checkout_failure "$fixture_repository" "tracked mutation"
windshare_run_isolated_git -C "$fixture_repository" checkout -- tracked.txt

printf 'untracked mutation\n' >"$fixture_repository/untracked.txt"
expect_checkout_failure "$fixture_repository" "untracked mutation"
rm -- "$fixture_repository/untracked.txt"

windshare_run_isolated_git -C "$fixture_repository" update-index --assume-unchanged tracked.txt
printf 'assume-unchanged mutation\n' >"$fixture_repository/tracked.txt"
expect_checkout_failure "$fixture_repository" "assume-unchanged mutation"
grep -Fq 'non-default Git index state' "$test_root/failure.log" ||
  fail "assume-unchanged failure did not identify hidden index state"
windshare_run_isolated_git -C "$fixture_repository" update-index --no-assume-unchanged tracked.txt
windshare_run_isolated_git -C "$fixture_repository" checkout -- tracked.txt

windshare_run_isolated_git -C "$fixture_repository" update-index --skip-worktree tracked.txt
printf 'skip-worktree mutation\n' >"$fixture_repository/tracked.txt"
expect_checkout_failure "$fixture_repository" "skip-worktree mutation"
grep -Fq 'non-default Git index state' "$test_root/failure.log" ||
  fail "skip-worktree failure did not identify hidden index state"
windshare_run_isolated_git -C "$fixture_repository" update-index --no-skip-worktree tracked.txt
windshare_run_isolated_git -C "$fixture_repository" checkout -- tracked.txt

windshare_assert_exact_release_file_projection "$linked_worktree" "$commit_sha" tracked.txt
printf 'linked tracked mutation\n' >"$linked_worktree/tracked.txt"
expect_checkout_failure "$linked_worktree" "linked tracked mutation"
windshare_run_isolated_git -C "$linked_worktree" checkout -- tracked.txt
printf 'linked untracked mutation\n' >"$linked_worktree/untracked.txt"
expect_checkout_failure "$linked_worktree" "linked untracked mutation"
rm -- "$linked_worktree/untracked.txt"
windshare_run_isolated_git -C "$linked_worktree" update-index --assume-unchanged tracked.txt
printf 'linked assume-unchanged mutation\n' >"$linked_worktree/tracked.txt"
expect_checkout_failure "$linked_worktree" "linked assume-unchanged mutation"
grep -Fq 'non-default Git index state' "$test_root/failure.log" ||
  fail "linked assume-unchanged failure did not identify hidden index state"
windshare_run_isolated_git -C "$linked_worktree" update-index --no-assume-unchanged tracked.txt
windshare_run_isolated_git -C "$linked_worktree" checkout -- tracked.txt
windshare_run_isolated_git -C "$linked_worktree" update-index --skip-worktree tracked.txt
printf 'linked skip-worktree mutation\n' >"$linked_worktree/tracked.txt"
expect_checkout_failure "$linked_worktree" "linked skip-worktree mutation"
grep -Fq 'non-default Git index state' "$test_root/failure.log" ||
  fail "linked skip-worktree failure did not identify hidden index state"
windshare_run_isolated_git -C "$linked_worktree" update-index --no-skip-worktree tracked.txt
windshare_run_isolated_git -C "$linked_worktree" checkout -- tracked.txt

windshare_create_exact_release_checkout \
  "$linked_worktree" \
  "$commit_sha" \
  "$exact_checkout" \
  tracked.txt
printf 'late source mutation\n' >"$linked_worktree/tracked.txt"
printf 'late untracked mutation\n' >"$linked_worktree/untracked.txt"
windshare_assert_exact_release_checkout "$exact_checkout" "$commit_sha"
expected_object="$(windshare_run_isolated_git -C "$fixture_repository" rev-parse "$commit_sha:tracked.txt")"
actual_object="$(windshare_run_isolated_git hash-object -- "$exact_checkout/tracked.txt")"
[ "$actual_object" = "$expected_object" ] || fail "private checkout consumed late source bytes"
printf 'post-projection mutation\n' >"$exact_checkout/tracked.txt"
if windshare_assert_exact_release_file_projection \
  "$exact_checkout" \
  "$commit_sha" \
  tracked.txt >"$test_root/post-projection-failure.log" 2>&1; then
  fail "post-projection mutation escaped raw revalidation"
fi
grep -Fq 'differs from its commit blob' "$test_root/post-projection-failure.log" ||
  fail "post-projection mutation failed for the wrong reason"

rm -- "$linked_worktree/untracked.txt"
printf '$Id$\n' >"$fixture_repository/tracked.txt"
printf 'tracked.txt ident\n' >"$fixture_repository/.gitattributes"
windshare_run_isolated_git -C "$fixture_repository" add -- .gitattributes tracked.txt
windshare_run_isolated_git -C "$fixture_repository" \
  -c user.name=WindShare -c user.email=release-contract.invalid \
  commit --quiet -m ident-filter
ident_commit="$(windshare_run_isolated_git -C "$fixture_repository" rev-parse HEAD)"
if windshare_create_exact_release_checkout \
  "$fixture_repository" \
  "$ident_commit" \
  "$test_root/ident-checkout" \
  tracked.txt >"$test_root/ident-failure.log" 2>&1; then
  fail "checkout transformation passed raw verifier projection"
fi
grep -Fq 'differs from its commit blob' "$test_root/ident-failure.log" ||
  fail "checkout transformation failed for the wrong reason"

trap - EXIT
rm -rf -- "$test_root"
echo "core release checkout contract: PASS"
