#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

repository_root="$(pwd -P)"
source "$repository_root/scripts/ci/core-release-linux-temp.sh"

release_root=""
unsafe_base=""
refused_root=""

cleanup() {
  local status="$?"
  trap - EXIT
  set +e
  if [ -n "$refused_root" ] && [ -d "$refused_root" ]; then
    rmdir -- "$refused_root"
  fi
  if [ -n "$unsafe_base" ] && [ -d "$unsafe_base" ]; then
    chmod 0700 -- "$unsafe_base"
    rmdir -- "$unsafe_base"
  fi
  if [ -n "$release_root" ] && [ -e "$release_root" ]; then
    windshare_linux_cleanup_release_environment
  fi
  exit "$status"
}
trap cleanup EXIT

fail() {
  echo "core-release Linux temp contract: $1" >&2
  exit 1
}

assert_contains() {
  local path="$1"
  local expected="$2"
  grep -Fq -- "$expected" "$path" || fail "$path is missing: $expected"
}

echo "-- Linux private release-root lifecycle"
windshare_linux_prepare_release_environment "$repository_root"
release_root="$WINDSHARE_LINUX_RELEASE_TEMP_ROOT"
case "$release_root" in
  "$repository_root"|"$repository_root"/*)
    fail "release root is inside the repository"
    ;;
esac
case "$TMPDIR" in
  "$release_root"/*) ;;
  *) fail "TMPDIR is not beneath the owned release root" ;;
esac
windshare_linux_assert_private_directory "$release_root" "test release root"
windshare_linux_assert_private_directory "$TMPDIR" "test TMPDIR"
windshare_linux_assert_ext4_ancestry "$TMPDIR" "test TMPDIR" >/dev/null

probe="$(mktemp -d "$TMPDIR/contract.XXXXXXXX")"
windshare_linux_assert_private_directory "$probe" "mktemp child"
rmdir -- "$probe"

unsafe_base="$WINDSHARE_LINUX_RELEASE_TEMP_PARENT/.windshare-linux-unsafe.$BASHPID"
install -d -m 0700 -- "$unsafe_base"
chmod 0770 -- "$unsafe_base"
if windshare_linux_assert_ext4_ancestry "$unsafe_base" "unsafe-mode fixture" >/dev/null 2>&1; then
  fail "group-writable ancestry passed validation"
fi
chmod 0700 -- "$unsafe_base"
rmdir -- "$unsafe_base"
unsafe_base=""

refused_root="$WINDSHARE_LINUX_RELEASE_TEMP_PARENT/not-a-windshare-release-root.$BASHPID"
install -d -m 0700 -- "$refused_root"
if windshare_linux_remove_release_root \
  "$refused_root" "$WINDSHARE_LINUX_RELEASE_TEMP_PARENT" >/dev/null 2>&1; then
  fail "cleanup accepted a path outside its owned namespace"
fi
[ -d "$refused_root" ] || fail "refused cleanup removed the foreign path"
rmdir -- "$refused_root"
refused_root=""

windshare_linux_cleanup_release_environment
[ ! -e "$release_root" ] || fail "owned release root survived cleanup"
release_root=""

echo "-- core-first release orchestration and no-replace invariant"
assert_contains ".github/workflows/ci.yml" '- "core/v*"'
assert_contains ".github/workflows/ci.yml" '- "core-candidate/v*/**"'
assert_contains ".github/workflows/ci.yml" 'run: bash scripts/ci/core-release.sh v0.3.0 "$GITHUB_SHA" linux-ext4'
assert_contains ".github/workflows/ci.yml" 'gowork-off-root:'
assert_contains ".github/workflows/core-release.yml" 'uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7'
assert_contains ".github/workflows/core-release.yml" 'uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/core-release-ref.tests.sh'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/core-release-ref.sh'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/core-release.sh "$CORE_RELEASE_VERSION" "$CORE_RELEASE_COMMIT_SHA" linux-ext4'
action_pin_pattern='^[[:space:]]*-[[:space:]]+uses:[[:space:]]+[^[:space:]#]+@[0-9a-f]{40}[[:space:]]+#[[:space:]]+v[0-9]+([.][0-9]+){0,2}[[:space:]]*$'
while IFS= read -r action; do
  if [[ ! "$action" =~ $action_pin_pattern ]]; then
    fail "core release workflow action is not pinned to a full SHA with a version comment: $action"
  fi
done < <(grep -E '^[[:space:]]*-[[:space:]]+uses:' .github/workflows/core-release.yml)
if [ "$(grep -Fc -- 'needs: candidate' .github/workflows/core-release.yml)" -ne 2 ]; then
  fail "native release jobs are not both bound to the resolved core candidate"
fi
if [ "$(grep -Fc -- 'go-version-file: core/go.mod' .github/workflows/core-release.yml)" -ne 2 ] ||
   [ "$(grep -Fc -- 'cache: false' .github/workflows/core-release.yml)" -ne 2 ] ||
   grep -Fq -- 'go-version-file: go.work' .github/workflows/core-release.yml; then
  fail "core release jobs do not derive an uncached toolchain from core/go.mod"
fi
ordinary_release_job="$(sed -n '/^  core-release:$/,/^  gowork-off-root:$/p' .github/workflows/ci.yml)"
if ! grep -Fq -- 'go-version-file: core/go.mod' <<<"$ordinary_release_job" ||
   ! grep -Fq -- 'cache: false' <<<"$ordinary_release_job" ||
   grep -Fq -- 'go-version-file: go.work' <<<"$ordinary_release_job"; then
  fail "ordinary CI core-release job does not use the uncached core toolchain"
fi
assert_contains "scripts/ci/core-release-ref.sh" 'object_type" != "commit"'
assert_contains "scripts/ci/core-release-ref.sh" 'require_direct_commit_ref "$candidate_ref" "$commit_sha"'
assert_contains "Makefile" 'bash scripts/ci/core-release.sh $(CORE_RELEASE_VERSION) $(CORE_RELEASE_COMMIT_SHA)'
assert_contains "scripts/ci/core-release.sh" 'native_profile="linux-ext4"'
assert_contains "scripts/ci/core-release.sh" 'export WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION="$native_profile"'
assert_contains "scripts/ci/core-release.sh" 'windshare_prepare_core_release_go_environment "$temporary_root"'
assert_contains "scripts/ci/core-release.sh" 'bash scripts/ci/core-release-checkout.tests.sh'
assert_contains "scripts/ci/core-release.sh" 'windshare_create_exact_release_checkout'
assert_contains "scripts/ci/core-release.sh" 'cd "$release_repository"'
assert_contains "scripts/ci/core-release.sh" '-commit "$release_commit"'
assert_contains "scripts/ci/core-release.ps1" 'Enter-CoreReleaseGoEnvironment -ReleaseRoot $temporaryRoot'
assert_contains "scripts/ci/core-release.ps1" "(Join-Path \$PSScriptRoot 'core-release-checkout.tests.ps1')"
assert_contains "scripts/ci/core-release.ps1" 'New-ExactCoreReleaseCheckout'
assert_contains "scripts/ci/core-release.ps1" 'Set-Location $releaseRepository'
assert_contains "scripts/ci/core-release.ps1" 'Assert-ExactCoreReleaseFileProjection'
assert_contains "scripts/ci/core-release.ps1" '-commit $CommitSHA'
assert_contains "scripts/ci/core-release-checkout.sh" 'GIT_*) unset "$variable_name"'
assert_contains "scripts/ci/core-release-checkout.sh" 'hash-object --no-filters'
assert_contains "scripts/ci/core-release-checkout.psm1" "StartsWith('GIT_', [StringComparison]::OrdinalIgnoreCase)"
assert_contains "scripts/ci/core-release-checkout.psm1" "'hash-object', '--no-filters'"
assert_contains "scripts/ci/_coremodulezip/main.go" '"ls-tree", "-r", "-z", "--full-tree", commitSHA'
assert_contains "scripts/ci/_coremodulezip/main.go" '"cat-file", "blob", objectID'
assert_contains "scripts/ci/core-release.sh" 'go run ./scripts/ci/_corevulnerability'
assert_contains "scripts/ci/core-release.sh" '-module "$artifact_root"'
assert_contains "scripts/ci/core-release.sh" '-cache "$temporary_root/vulnerability-cache"'
assert_contains "scripts/ci/core-release.ps1" 'go run ./scripts/ci/_corevulnerability'
assert_contains "scripts/ci/core-release.ps1" '-module $artifactRoot'
assert_contains "scripts/ci/core-release.ps1" "-cache (Join-Path \$temporaryRoot 'vulnerability-cache')"
assert_contains "scripts/ci/_corevulnerability/main.go" 'golang.org/x/vuln/cmd/govulncheck@v1.6.0'
assert_contains "scripts/ci/_corevulnerability/main.go" '"GOPROXY=" + publicGoProxy'
assert_contains "scripts/ci/_corevulnerability/main.go" '"GOSUMDB=" + publicGoChecksumDatabase'
assert_contains "scripts/ci/core-release.sh" 'coverage_tool="github.com/vladopajic/go-test-coverage/v2@v2.18.8"'
assert_contains "scripts/ci/core-release.ps1" "\$coverageTool = 'github.com/vladopajic/go-test-coverage/v2@v2.18.8'"
if grep -Fq -- 'GO_TEST_COVERAGE' scripts/ci/core-release.sh scripts/ci/core-release.ps1; then
  fail "core release coverage verifier still accepts a caller override"
fi

for module_file in go.mod core/go.mod; do
  if grep -Eq '^[[:space:]]*replace([[:space:]]|\()' "$module_file"; then
    fail "$module_file contains a replace directive"
  fi
done

trap - EXIT
echo "core-release Linux temp contract: PASS"
