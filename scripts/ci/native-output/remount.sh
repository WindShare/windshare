#!/usr/bin/env bash
set -euo pipefail

filesystem="${1:?expected vfat or exfat}"
case "$filesystem" in
  vfat|exfat) ;;
  *) echo "unsupported test filesystem: $filesystem" >&2; exit 2 ;;
esac

workspace="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/windshare-remount.XXXXXX")"
image="$workspace/volume.img"
mount_dir="$workspace/mount"
binary="$workspace/osfs.test"
mkdir "$mount_dir"
truncate -s 128M "$image"
# Format only our newly-created regular image, never a host device.
case "$filesystem" in
  vfat) mkfs.vfat -F 32 "$image" ;;
  exfat) mkfs.exfat "$image" ;;
esac
go test -c -o "$binary" ./osfs

cleanup() {
  if mountpoint -q "$mount_dir"; then sudo umount "$mount_dir"; fi
}
trap cleanup EXIT
mount_image() {
  sudo mount -t "$filesystem" -o "loop,uid=$(id -u),gid=$(id -g)" "$image" "$mount_dir"
}
run_phase() {
  WINDSHARE_REMOUNT_ROOT="$mount_dir" WINDSHARE_REMOUNT_PHASE="$1" \
    "$binary" -test.run '^TestPortableFilesystemRemount$' -test.v -test.count=1 -test.timeout=60s
}
mount_image
run_phase create
sudo umount "$mount_dir"
mount_image
run_phase recover
