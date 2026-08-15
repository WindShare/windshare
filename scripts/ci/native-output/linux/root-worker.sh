#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 6 ]; then
  echo "usage: root-worker.sh <loop-device> <mountpoint> <osfs-test-binary> <outputlinux-test-binary> <uid> <gid>" >&2
  exit 2
fi

loop_device="$1"
mountpoint_path="$2"
osfs_test_binary="$3"
outputlinux_test_binary="$4"
receiver_uid="$5"
receiver_gid="$6"

if [ "$(id -u)" -ne 0 ]; then
  echo "Linux native root worker requires root only for its isolated fixture" >&2
  exit 1
fi
if [[ ! "$loop_device" =~ ^/dev/loop[0-9]+$ ]] ||
   [[ ! "$receiver_uid" =~ ^[1-9][0-9]*$ ]] ||
   [[ ! "$receiver_gid" =~ ^[1-9][0-9]*$ ]]; then
  echo "Linux native root worker received an invalid fixed identity or loop device" >&2
  exit 2
fi
if [ ! -d "$mountpoint_path" ] || [ -L "$mountpoint_path" ] ||
   [ ! -f "$osfs_test_binary" ] || [ -L "$osfs_test_binary" ] ||
   [ ! -f "$outputlinux_test_binary" ] || [ -L "$outputlinux_test_binary" ]; then
  echo "Linux native root worker requires no-follow fixture paths" >&2
  exit 2
fi
mountpoint_path="$(cd -- "$mountpoint_path" && pwd -P)"
osfs_test_binary="$(cd -- "$(dirname -- "$osfs_test_binary")" && pwd -P)/$(basename -- "$osfs_test_binary")"
outputlinux_test_binary="$(cd -- "$(dirname -- "$outputlinux_test_binary")" && pwd -P)/$(basename -- "$outputlinux_test_binary")"

proc_mounted=false
root_mounted=false
cleanup() {
  local status="$?"
  trap - EXIT INT TERM
  set +e
  if [ "$proc_mounted" = true ]; then
    umount -- "$mountpoint_path/proc"
    [ "$?" -eq 0 ] || status=1
  fi
  if [ "$root_mounted" = true ]; then
    if ! sync -f "$mountpoint_path"; then
      status=1
    fi
    umount -- "$mountpoint_path"
    [ "$?" -eq 0 ] || status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mount -t ext4 -o nodev,nosuid -- "$loop_device" "$mountpoint_path"
root_mounted=true
install -d -o 0 -g 0 -m 0755 -- \
  "$mountpoint_path/proc" \
  "$mountpoint_path/test"
install -o 0 -g 0 -m 0555 -- "$osfs_test_binary" "$mountpoint_path/test/osfs.test"
install -o 0 -g 0 -m 0555 -- "$outputlinux_test_binary" "$mountpoint_path/test/outputlinux.test"
install -d -o "$receiver_uid" -g "$receiver_gid" -m 0700 -- \
  "$mountpoint_path/receiver" \
  "$mountpoint_path/receiver/test-temp" \
  "$mountpoint_path/receiver/reuse" \
  "$mountpoint_path/receiver/reuse/fillers"
mount -t proc -o nodev,nosuid,noexec -- proc "$mountpoint_path/proc"
proc_mounted=true

umask 0022
run_native_test_binary() {
  local binary_path="$1"
  local test_pattern="$2"

  TMPDIR=/receiver/test-temp \
  WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION=linux-ext4 \
  WINDSHARE_LINUX_NATIVE_FIXTURE=loop-ext4-v1 \
  WINDSHARE_LINUX_NATIVE_TEMP_ROOT=/receiver/test-temp \
  WINDSHARE_LINUX_NATIVE_REUSE_ROOT=/receiver/reuse \
    chroot \
      --userspec="$receiver_uid:$receiver_gid" \
      --groups="$receiver_gid" \
      "$mountpoint_path" \
      "$binary_path" \
        -test.v=test2json \
        -test.count=1 \
        -test.timeout=20m \
        -test.run="$test_pattern"
}

# The moved identity test is compiled from internal/outputlinux; the two
# broader filesystem certification tests remain in osfs. Running both binaries
# in one mounted fixture preserves the inode-reuse evidence while respecting
# package ownership.
run_native_test_binary /test/outputlinux.test '^TestLinuxExt4RestartIdentityRejectsForcedInodeReuse$'
run_native_test_binary /test/osfs.test '^(TestLinuxExt4NativeCertification|TestLinuxExt4ProcessRestartRecovery)$'
