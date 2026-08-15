#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../../.."

repository_root="$(pwd -P)"
source "$repository_root/scripts/ci/native-output/linux/environment.sh"

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
  echo "Linux native-output environment contract: $1" >&2
  exit 1
}

assert_contains() {
  local file_path="$1"
  local expected="$2"
  grep -Fq -- "$expected" "$file_path" || fail "$file_path is missing: $expected"
}

echo "-- Linux private certification-root lifecycle"
windshare_linux_prepare_release_environment "$repository_root"
release_root="$WINDSHARE_LINUX_RELEASE_TEMP_ROOT"
case "$release_root" in
  "$repository_root"|"$repository_root"/*)
    fail "certification root is inside the repository"
    ;;
esac
case "$TMPDIR" in
  "$release_root"/*) ;;
  *) fail "TMPDIR is not beneath the owned certification root" ;;
esac
windshare_linux_assert_private_directory "$release_root" "test certification root"
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
if windshare_linux_remove_release_root   "$refused_root" "$WINDSHARE_LINUX_RELEASE_TEMP_PARENT" >/dev/null 2>&1; then
  fail "cleanup accepted a path outside its owned namespace"
fi
[ -d "$refused_root" ] || fail "refused cleanup removed the foreign path"
rmdir -- "$refused_root"
refused_root=""

preserve_marker="$release_root/.windshare-preserve-native-fixture"
: >"$preserve_marker"
if windshare_linux_remove_release_root   "$release_root" "$WINDSHARE_LINUX_RELEASE_TEMP_PARENT" >/dev/null 2>&1; then
  fail "cleanup accepted a fixture with unproven loop detachment"
fi
[ -d "$release_root" ] || fail "preserved fixture was removed"
rm -- "$preserve_marker"

windshare_linux_cleanup_release_environment
[ ! -e "$release_root" ] || fail "owned certification root survived cleanup"
release_root=""

echo "-- Linux loop-ext4 certification contract"
certifier="scripts/ci/native-output/linux/certify.sh"
root_worker="scripts/ci/native-output/linux/root-worker.sh"
assert_contains "$certifier" 'compile_static_test_binary ./core/osfs "$osfs_test_binary"'
assert_contains "$certifier" 'compile_static_test_binary ./core/osfs/internal/outputlinux "$outputlinux_test_binary"'
assert_contains "$certifier" 'mkfs.ext4 -q -F -N 1024 -I 256 -m 0'
assert_contains "$certifier" 'sudo -n unshare --mount --propagation private --fork --kill-child'
assert_contains "$certifier" 'bash "$script_root/root-worker.sh"'
assert_contains "$certifier" '.windshare-preserve-native-fixture'
assert_contains "$certifier" 'TestLinuxExt4RestartIdentityRejectsForcedInodeReuse'
assert_contains "$certifier" 'TestLinuxExt4NativeCertification'
assert_contains "$certifier" 'TestLinuxExt4ProcessRestartRecovery'
assert_contains "$certifier" '"Action":"skip"'
assert_contains "$certifier" '"Action":"fail"'
assert_contains "$root_worker" 'WINDSHARE_LINUX_NATIVE_FIXTURE=loop-ext4-v1'
assert_contains "$root_worker" '--userspec="$receiver_uid:$receiver_gid"'
assert_contains "$root_worker" '/test/osfs.test'
assert_contains "$root_worker" '/test/outputlinux.test'

echo "Linux native-output environment tests: PASS"
