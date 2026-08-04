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
assert_not_contains ".github/workflows/ci.yml" '- "core/v*"'
assert_not_contains ".github/workflows/ci.yml" '- "core-candidate/v*/**"'
assert_not_contains ".github/workflows/core-release.yml" '- "core/v*"'
assert_contains ".github/workflows/core-release.yml" '- "core-candidate/v*/**"'
assert_contains ".github/workflows/core-release.yml" 'uses: actions/checkout@v7'
assert_contains ".github/workflows/core-release.yml" 'uses: actions/setup-go@v7'
assert_not_contains ".github/workflows/core-release.yml" 'core-candidate/v0.3.0'
assert_contains "scripts/ci/linux/vet.sh" 'GOWORK=off go build ./...'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/core-release-ref.tests.sh'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/core-release-ref.sh resolve'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/core-release-ref.sh publish'
assert_contains ".github/workflows/core-release.yml" 'run: bash scripts/ci/linux/core-release.sh "$CORE_RELEASE_VERSION" "$CORE_RELEASE_COMMIT_SHA" linux-ext4'
assert_contains ".github/workflows/core-release.yml" "if: github.event_name == 'push'"
assert_contains ".github/workflows/core-release.yml" 'group: core-release-linux-${{ needs.candidate.outputs.version }}'
assert_contains ".github/workflows/core-release.yml" 'group: core-release-windows-${{ needs.candidate.outputs.version }}'
assert_contains ".github/workflows/core-release.yml" 'group: core-release-publish-${{ needs.candidate.outputs.version }}'
manual_version_input="$(sed -n '/^      version:$/,/^        type: string$/p' .github/workflows/core-release.yml)"
if [ -z "$manual_version_input" ] ||
   ! grep -Fq -- 'required: true' <<<"$manual_version_input" ||
   grep -Fq -- 'default:' <<<"$manual_version_input"; then
  fail "manual diagnostic version must be required and have no default"
fi
if grep -Eq 'uses:[[:space:]]+[^[:space:]#]+@[0-9a-f]{40}' .github/workflows/core-release.yml; then
  fail "core release workflow retains a commit-SHA action pin"
fi
if [ "$(grep -Fc -- 'contents: write' .github/workflows/core-release.yml)" -ne 1 ]; then
  fail "exactly one core release job must receive contents: write"
fi
if [ "$(grep -Fc -- 'needs: candidate' .github/workflows/core-release.yml)" -ne 2 ]; then
  fail "native release jobs are not both bound to the resolved core candidate"
fi
if [ "$(grep -Fc -- 'go-version: stable' .github/workflows/core-release.yml)" -ne 2 ] ||
   [ "$(grep -Fc -- 'check-latest: true' .github/workflows/core-release.yml)" -ne 2 ] ||
   [ "$(grep -Fc -- 'cache: false' .github/workflows/core-release.yml)" -ne 2 ] ||
   grep -Fq -- 'go-version-file:' .github/workflows/core-release.yml; then
  fail "core release jobs do not use the latest stable uncached Go toolchain"
fi
if [ "$(grep -Fc -- 'go install golang.org/x/vuln/cmd/govulncheck@latest' .github/workflows/core-release.yml)" -ne 2 ]; then
  fail "hosted release jobs do not each provision the latest govulncheck"
fi
assert_contains "scripts/ci/core-release-ref.sh" 'git push "$remote_name" "$commit_sha:$release_ref"'
assert_contains "Makefile" 'override CORE_RELEASE_VERSION := v0.0.0-ci'
assert_contains "Makefile" 'bash scripts/ci/linux/core-release.sh "$(CORE_RELEASE_VERSION)" "$(CORE_RELEASE_COMMIT)"'
assert_not_contains "Makefile" 'CORE_ARTIFACT_VERSION'
make_preview="$(make -n core-release CORE_RELEASE_VERSION=v0.3.0)"
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
assert_contains "scripts/ci/linux/core-release.sh" 'command -v govulncheck'
assert_contains "scripts/ci/linux/core-release.sh" '"$govulncheck_executable" ./...'
assert_not_contains "scripts/ci/linux/core-release.sh" 'go install'
assert_contains "scripts/ci/windows/core-release.ps1" 'Get-Command govulncheck -CommandType Application'
assert_contains "scripts/ci/windows/core-release.ps1" '& $govulncheckExecutable ./...'
assert_not_contains "scripts/ci/windows/core-release.ps1" 'go install'
if [ -e scripts/ci/_corevulnerability ]; then
  fail "retired repository-owned vulnerability scanner wrapper still exists"
fi
if grep -Eq -- 'govulncheck@v[0-9]' \
  .github/workflows/core-release.yml \
  scripts/ci/linux/core-release.sh \
  scripts/ci/windows/core-release.ps1; then
  fail "core release retains an exact govulncheck version"
fi
assert_contains "scripts/ci/linux/core-release.sh" 'core_suite_test_timeout="30m"'
assert_contains "scripts/ci/linux/core-release.sh" 'go test -count=1 -timeout="$core_suite_test_timeout" ./...'
if grep -Eq -- 'go test -race|-covermode=atomic|go-test-coverage' \
  scripts/ci/linux/core-release.sh scripts/ci/windows/core-release.ps1; then
  fail "core release duplicates the race or coverage instrumentation owners"
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
