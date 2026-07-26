#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 5 ]; then
  echo "usage: core-release-linux-native-root.sh <loop-device> <mountpoint> <test-binary> <uid> <gid>" >&2
  exit 2
fi

loop_device="$1"
mountpoint_path="$2"
test_binary="$3"
receiver_uid="$4"
receiver_gid="$5"

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
   [ ! -f "$test_binary" ] || [ -L "$test_binary" ]; then
  echo "Linux native root worker requires no-follow fixture paths" >&2
  exit 2
fi
mountpoint_path="$(cd -- "$mountpoint_path" && pwd -P)"
test_binary="$(cd -- "$(dirname -- "$test_binary")" && pwd -P)/$(basename -- "$test_binary")"

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
install -o 0 -g 0 -m 0555 -- "$test_binary" "$mountpoint_path/test/osfs.test"
install -d -o "$receiver_uid" -g "$receiver_gid" -m 0700 -- \
  "$mountpoint_path/receiver" \
  "$mountpoint_path/receiver/test-temp" \
  "$mountpoint_path/receiver/reuse" \
  "$mountpoint_path/receiver/reuse/fillers"
mount -t proc -o nodev,nosuid,noexec -- proc "$mountpoint_path/proc"
proc_mounted=true

umask 0022
TMPDIR=/receiver/test-temp \
WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION=linux-ext4 \
WINDSHARE_LINUX_NATIVE_FIXTURE=loop-ext4-v1 \
WINDSHARE_LINUX_NATIVE_TEMP_ROOT=/receiver/test-temp \
WINDSHARE_LINUX_NATIVE_REUSE_ROOT=/receiver/reuse \
  chroot \
    --userspec="$receiver_uid:$receiver_gid" \
    --groups="$receiver_gid" \
    "$mountpoint_path" \
    /test/osfs.test \
      -test.v=test2json \
      -test.count=1 \
      -test.timeout=20m \
      -test.run='^(TestLinuxExt4RestartIdentityRejectsForcedInodeReuse|TestLinuxExt4NativeCertification|TestLinuxExt4ProcessRestartRecovery)$'
