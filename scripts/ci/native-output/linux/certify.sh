#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: scripts/ci/native-output/linux/certify.sh <module-root> <private-certification-root>" >&2
  exit 2
fi

artifact_root="$1"
release_root="$2"
script_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
if [ "$(uname -s)" != "Linux" ] || [ "$(id -u)" -eq 0 ]; then
  echo "Linux/ext4 native certification requires an unprivileged Linux orchestrator" >&2
  exit 1
fi
if [ ! -d "$artifact_root" ] || [ -L "$artifact_root" ] ||
   [ ! -d "$release_root" ] || [ -L "$release_root" ]; then
  echo "Linux/ext4 native certification requires no-follow input directories" >&2
  exit 2
fi
artifact_root="$(cd -- "$artifact_root" && pwd -P)"
release_root="$(cd -- "$release_root" && pwd -P)"

if [ -z "${WINDSHARE_GO_EXECUTABLE:-}" ] || [ ! -r "$WINDSHARE_GO_EXECUTABLE" ]; then
  echo 'Linux/ext4 native certification requires the retained coordinator Go application' >&2
  exit 1
fi
for command_name in readelf truncate mkfs.ext4 losetup udevadm sudo unshare chroot timeout; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "Linux/ext4 native certification is missing required command: $command_name" >&2
    exit 1
  fi
done
if ! sudo -n true; then
  echo "Linux/ext4 native certification requires passwordless sudo for loop/mount isolation" >&2
  exit 1
fi

native_root="$release_root/linux-native"
if [ -e "$native_root" ] || [ -L "$native_root" ]; then
  echo "Linux native fixture root already exists: $native_root" >&2
  exit 1
fi
install -d -m 0700 -- "$native_root"
osfs_test_binary="$native_root/osfs.test"
outputlinux_test_binary="$native_root/outputlinux.test"
image="$native_root/ext4.img"
mountpoint_path="$native_root/mount"
events="$native_root/events.json"
preserve_marker="$release_root/.windshare-preserve-native-fixture"
install -d -m 0700 -- "$mountpoint_path"

compile_static_test_binary() {
  local package_path="$1"
  local binary_path="$2"

  CGO_ENABLED=0 GOWORK=off "$WINDSHARE_GO_EXECUTABLE" -C "$artifact_root" test -c -o "$binary_path" "$package_path"
  if readelf -l "$binary_path" | grep -Fq 'INTERP'; then
    echo "Linux native certification binary is dynamically linked: $package_path" >&2
    exit 1
  fi
}

# The restart-identity contract lives with the Linux adapter package. Keep it
# in its native package rather than copying the test into osfs, so the release
# fixture executes the same package boundary that production code uses.
echo "-- compile static Linux native certification binaries"
compile_static_test_binary ./core/osfs "$osfs_test_binary"
compile_static_test_binary ./core/osfs/internal/outputlinux "$outputlinux_test_binary"

echo "-- construct deterministic tiny-inode ext4 fixture"
truncate -s 128M -- "$image"
mkfs.ext4 -q -F -N 1024 -I 256 -m 0 \
  -E lazy_itable_init=0,lazy_journal_init=0 \
  -U random "$image"

loop_device=""
cleanup() {
  local status="$?"
  local detach_status=0
  local loop_query=""
  local query_status=0
  trap - EXIT INT TERM
  set +e
  if [ -e "$preserve_marker" ]; then
    loop_query="$(sudo -n losetup -j "$image" 2>&1)"
    query_status="$?"
    if [ "$query_status" -ne 0 ]; then
      detach_status=1
    elif [ -n "$loop_query" ]; then
      if [[ ! "$loop_device" =~ ^/dev/loop[0-9]+$ ]] ||
         ! sudo -n losetup -d "$loop_device"; then
        detach_status=1
      fi
      sudo -n udevadm settle || detach_status=1
      loop_query="$(sudo -n losetup -j "$image" 2>&1)"
      query_status="$?"
      if [ "$query_status" -ne 0 ] || [ -n "$loop_query" ]; then
        detach_status=1
      fi
    fi
  fi
  if [ "$detach_status" -ne 0 ]; then
    echo "failed to prove loop-device detachment; preserving Linux native fixture" >&2
    status=1
  else
    rm -f -- "$preserve_marker"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The marker makes abrupt termination fail safe: the outer release cleanup will
# refuse to remove the backing image until this script proves detachment.
: >"$preserve_marker"
loop_device="$(sudo -n losetup --find --show --nooverlap "$image")"
if [[ ! "$loop_device" =~ ^/dev/loop[0-9]+$ ]]; then
  echo "losetup returned an invalid loop device: $loop_device" >&2
  exit 1
fi

echo "-- required loop-ext4 inode-reuse and process-restart certification"
set +e
timeout --signal=TERM --kill-after=30s 25m \
  sudo -n unshare --mount --propagation private --fork --kill-child \
    bash "$script_root/root-worker.sh" \
      "$loop_device" "$mountpoint_path" "$osfs_test_binary" "$outputlinux_test_binary" \
      "$(id -u)" "$(id -g)" \
  | "$WINDSHARE_GO_EXECUTABLE" tool test2json -t -p github.com/windshare/windshare/core/osfs \
  | tee "$events"
pipeline_status=("${PIPESTATUS[@]}")
set -e
for status in "${pipeline_status[@]}"; do
  if [ "$status" -ne 0 ]; then
    echo "required loop-ext4 native certification pipeline failed: ${pipeline_status[*]}" >&2
    exit "$status"
  fi
done
if grep -Fq '"Action":"skip"' "$events" || grep -Fq '"Action":"fail"' "$events"; then
  echo "required loop-ext4 native certification reported SKIP or FAIL" >&2
  exit 1
fi
for required_test in \
  TestLinuxExt4RestartIdentityRejectsForcedInodeReuse \
  TestLinuxExt4NativeCertification \
  TestLinuxExt4ProcessRestartRecovery; do
  if [ "$(grep -Ec "\"Action\":\"pass\".*\"Test\":\"${required_test}\"" "$events")" -ne 1 ]; then
    echo "required native test did not report exactly one PASS: $required_test" >&2
    exit 1
  fi
done

echo "-- loop-ext4 native certification passed"
