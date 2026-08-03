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

assert_not_contains() {
  local path="$1"
  local forbidden="$2"
  if grep -Fq -- "$forbidden" "$path"; then
    fail "$path contains forbidden text: $forbidden"
  fi
}

workflow_job_source() {
  local path="$1"
  local job="$2"
  awk -v header="  ${job}:" '
    $0 == header { capture = 1 }
    capture && $0 != header && $0 ~ /^  [a-z0-9][a-z0-9-]*:$/ { exit }
    capture { print }
  ' "$path"
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

preserve_marker="$release_root/.windshare-preserve-native-fixture"
: >"$preserve_marker"
if windshare_linux_remove_release_root \
  "$release_root" "$WINDSHARE_LINUX_RELEASE_TEMP_PARENT" >/dev/null 2>&1; then
  fail "cleanup accepted a native fixture with unproven loop detachment"
fi
[ -d "$release_root" ] || fail "preserved native fixture was removed"
rm -- "$preserve_marker"

windshare_linux_cleanup_release_environment
[ ! -e "$release_root" ] || fail "owned release root survived cleanup"
release_root=""

echo "-- core-first release orchestration and no-replace invariant"
assert_contains ".github/workflows/ci.yml" '- "core/v*"'
assert_contains ".github/workflows/ci.yml" '- "core-candidate/v*/**"'
assert_contains ".github/workflows/current-commit.yml" 'CORE_ARTIFACT_VERSION: "v0.0.0-ci"'
assert_contains ".github/workflows/current-commit.yml" 'run: bash scripts/ci/linux/core-release.sh "$CORE_ARTIFACT_VERSION" "$GITHUB_SHA" linux-ext4'
assert_not_contains ".github/workflows/current-commit.yml" 'core-release.sh v0.3.0'
assert_not_contains ".github/workflows/current-commit.yml" 'gowork-off-root:'
assert_contains "scripts/ci/linux/vet.sh" 'GOWORK=off windshare_go build ./...'
assert_contains ".github/workflows/core-release.yml" 'uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7'
assert_contains ".github/workflows/core-release.yml" 'uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6'
assert_contains ".github/workflows/core-release.yml" '- "!core/v0.3.0"'
assert_contains ".github/workflows/core-release.yml" '- "!core-candidate/v0.3.0/**"'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/core-release-ref.tests.sh'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/core-release-ref.sh'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/linux/core-release.sh "$CORE_RELEASE_VERSION" "$CORE_RELEASE_COMMIT_SHA" linux-ext4'
manual_version_input="$(sed -n '/^      version:$/,/^        type: string$/p' .github/workflows/core-release.yml)"
if [ -z "$manual_version_input" ] ||
   ! grep -Fq -- 'required: true' <<<"$manual_version_input" ||
   grep -Fq -- 'default:' <<<"$manual_version_input"; then
  fail "manual release version must be required and have no default"
fi
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
ordinary_release_job="$(workflow_job_source .github/workflows/current-commit.yml core-release)"
if ! grep -Fq -- 'go-version-file: core/go.mod' <<<"$ordinary_release_job" ||
   ! grep -Fq -- 'cache: false' <<<"$ordinary_release_job" ||
   ! grep -Fq -- 'timeout-minutes: 60' <<<"$ordinary_release_job" ||
   grep -Fq -- 'go-version-file: go.work' <<<"$ordinary_release_job"; then
  fail "ordinary CI core-release job lacks the fixed timeout or uncached core toolchain"
fi
assert_contains "scripts/ci/core-release-ref.sh" 'object_type" != "commit"'
assert_contains "scripts/ci/core-release-ref.sh" 'require_direct_commit_ref "$candidate_ref" "$commit_sha"'
assert_contains "Makefile" 'override CORE_ARTIFACT_VERSION := v0.0.0-ci'
assert_contains "Makefile" '"$(WINDSHARE_BASH_EXECUTABLE)" scripts/ci/linux/core-release.sh "$(CORE_ARTIFACT_VERSION)" "$(CORE_ARTIFACT_COMMIT_SHA)"'
assert_not_contains "Makefile" 'CORE_RELEASE_VERSION'
make_preview="$(make -n core-release CORE_ARTIFACT_VERSION=v0.3.0)"
if ! grep -Fq -- 'v0.0.0-ci' <<<"$make_preview" ||
   grep -Fq -- 'v0.3.0' <<<"$make_preview"; then
  fail "core-release make target allows its synthetic artifact version to be overridden"
fi
assert_contains "scripts/ci/linux/core-release.sh" 'native_profile="linux-ext4"'
assert_contains "scripts/ci/linux/core-release.sh" 'unset WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION'
assert_contains "scripts/ci/linux/core-release.sh" 'bash scripts/ci/core-release-linux-native.sh "$artifact_root" "$temporary_root"'
assert_contains "scripts/ci/core-release-linux-native.sh" 'compile_static_test_binary ./osfs "$osfs_test_binary"'
assert_contains "scripts/ci/core-release-linux-native.sh" 'compile_static_test_binary ./osfs/internal/outputlinux "$outputlinux_test_binary"'
assert_contains "scripts/ci/core-release-linux-native.sh" 'mkfs.ext4 -q -F -N 1024 -I 256 -m 0'
assert_contains "scripts/ci/core-release-linux-native.sh" 'sudo -n unshare --mount --propagation private --fork --kill-child'
assert_contains "scripts/ci/core-release-linux-native.sh" '"$WINDSHARE_GO_EXECUTABLE" tool test2json -t -p github.com/windshare/windshare/core/osfs'
assert_contains "scripts/ci/core-release-linux-native.sh" 'TestLinuxExt4RestartIdentityRejectsForcedInodeReuse'
assert_contains "scripts/ci/core-release-linux-native.sh" 'TestLinuxExt4NativeCertification'
assert_contains "scripts/ci/core-release-linux-native.sh" 'TestLinuxExt4ProcessRestartRecovery'
assert_contains "scripts/ci/core-release-linux-native.sh" ': >"$preserve_marker"'
assert_contains "scripts/ci/core-release-linux-native.sh" 'sudo -n udevadm settle'
assert_contains "scripts/ci/core-release-linux-native.sh" 'sudo -n losetup -d "$loop_device"'
assert_contains "core/osfs/internal/outputlinux/linux_output_persistent_identity_native_test.go" 'command.Stdin = bytes.NewReader(nil)'
assert_contains "core/osfs/output_v3_native_certification_test.go" 'command.Stdin = bytes.NewReader(nil)'
assert_contains "scripts/ci/core-release-linux-native-root.sh" 'WINDSHARE_LINUX_NATIVE_FIXTURE=loop-ext4-v1'
assert_contains "scripts/ci/core-release-linux-native-root.sh" '--userspec="$receiver_uid:$receiver_gid"'
assert_contains "scripts/ci/core-release-linux-native-root.sh" '/test/osfs.test'
assert_contains "scripts/ci/core-release-linux-native-root.sh" '/test/outputlinux.test'
if grep -Fq -- 'losetup -d --' scripts/ci/core-release-linux-native.sh; then
  fail "Linux native certification passes an option terminator as a loop device"
fi
assert_contains "scripts/ci/linux/core-release.sh" 'windshare_prepare_core_release_go_environment "$temporary_root"'
assert_contains "scripts/ci/linux/core-release.sh" 'bash scripts/ci/core-release-checkout.tests.sh'
assert_contains "scripts/ci/linux/core-release.sh" 'windshare_create_exact_release_checkout'
assert_contains "scripts/ci/linux/core-release.sh" 'cd "$release_repository"'
assert_contains "scripts/ci/linux/core-release.sh" '-commit "$release_commit"'
assert_contains "scripts/ci/windows/core-release.ps1" 'Enter-CoreReleaseGoEnvironment -ReleaseRoot $temporaryRoot'
assert_contains "scripts/ci/windows/core-release.ps1" "(Join-Path \$ciRoot 'core-release-checkout.tests.ps1')"
assert_contains "scripts/ci/windows/core-release.ps1" 'New-ExactCoreReleaseCheckout'
assert_contains "scripts/ci/windows/core-release.ps1" 'Set-Location $releaseRepository'
assert_contains "scripts/ci/windows/core-release.ps1" 'Assert-ExactCoreReleaseFileProjection'
assert_contains "scripts/ci/windows/core-release.ps1" '-commit $CommitSHA'
assert_contains "scripts/ci/core-release-checkout.sh" 'GIT_*) unset "$variable_name"'
assert_contains "scripts/ci/core-release-checkout.sh" 'hash-object --no-filters'
assert_contains "scripts/ci/core-release-checkout.sh" 'scripts/ci/core-release-linux-native.sh'
assert_contains "scripts/ci/core-release-checkout.sh" 'scripts/ci/core-release-linux-native-root.sh'
assert_contains "scripts/ci/core-release-checkout.psm1" "StartsWith('GIT_', [StringComparison]::OrdinalIgnoreCase)"
assert_contains "scripts/ci/core-release-checkout.psm1" "'hash-object', '--no-filters'"
assert_contains "scripts/ci/_coremodulezip/main.go" '"ls-tree", "-r", "-z", "--full-tree", commitSHA'
assert_contains "scripts/ci/_coremodulezip/main.go" '"cat-file", "blob", objectID'
assert_contains "scripts/ci/linux/core-release.sh" 'GOWORK=off windshare_go run ./scripts/ci/_corevulnerability'
assert_contains "scripts/ci/linux/core-release.sh" '-module "$artifact_root"'
assert_contains "scripts/ci/linux/core-release.sh" '-cache "$temporary_root/vulnerability-cache"'
assert_contains "scripts/ci/windows/core-release.ps1" 'Invoke-WindShareGo run ./scripts/ci/_corevulnerability'
assert_contains "scripts/ci/windows/core-release.ps1" '-module $artifactRoot'
assert_contains "scripts/ci/windows/core-release.ps1" "-cache (Join-Path \$temporaryRoot 'vulnerability-cache')"
assert_contains "scripts/ci/_corevulnerability/main.go" 'golang.org/x/vuln/cmd/govulncheck@v1.6.0'
assert_contains "scripts/ci/_corevulnerability/main.go" '"GOPROXY=" + publicGoProxy'
assert_contains "scripts/ci/_corevulnerability/main.go" '"GOSUMDB=" + publicGoChecksumDatabase'
assert_contains "scripts/ci/linux/core-release.sh" 'core_suite_test_timeout="30m"'
assert_contains "scripts/ci/linux/core-release.sh" 'go test -count=1 -timeout="$core_suite_test_timeout" ./...'
if grep -Eq -- 'go test -race|-covermode=atomic|go-test-coverage' \
  scripts/ci/linux/core-release.sh scripts/ci/windows/core-release.ps1; then
  fail "core release duplicates the race or coverage instrumentation authorities"
fi
if grep -Fq -- 'GO_TEST_COVERAGE' scripts/ci/linux/core-release.sh scripts/ci/windows/core-release.ps1; then
  fail "core release coverage verifier still accepts a caller override"
fi
if grep -Fq -- '--autoclear' scripts/ci/core-release-linux-native.sh; then
  fail "Linux native certification relies on an unsupported implicit loop lifecycle"
fi

for module_file in go.mod core/go.mod; do
  if grep -Eq '^[[:space:]]*replace([[:space:]]|\()' "$module_file"; then
    fail "$module_file contains a replace directive"
  fi
done

trap - EXIT
echo "core-release Linux temp contract: PASS"
