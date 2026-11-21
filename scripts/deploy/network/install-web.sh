#!/usr/bin/env bash
set -euo pipefail

artifact=${1:?artifact path required}
expected=${2:?SHA-256 required}
revision=${3:?40-character commit required}
[[ $expected =~ ^[0-9a-f]{64}$ && $revision =~ ^[0-9a-f]{40}$ ]] || exit 2
[[ $(id -u) == 0 && -f $artifact && ! -L $artifact ]] || exit 2
readonly root=/srv/windshare
readonly retain_releases=3
install -d -m 0755 "$root/releases"
exec 9>/run/lock/windshare-web.lock
flock 9
actual=$(sha256sum "$artifact")
[[ ${actual%% *} == "$expected" ]] || { echo 'Web artifact checksum mismatch' >&2; exit 1; }
candidate=$(mktemp -d "$root/releases/.stage.XXXXXXXX")
trap 'rm -rf -- "$candidate"' EXIT

# Archives come from CI, but extraction still rejects links and paths escaping staging.
python3 - "$artifact" "$candidate" <<'PY'
import pathlib, sys, tarfile
with tarfile.open(sys.argv[1], "r:gz") as archive:
    members = archive.getmembers()
    for member in members:
        path = pathlib.PurePosixPath(member.name)
        if path.is_absolute() or ".." in path.parts or not (member.isfile() or member.isdir()):
            raise SystemExit("Unsafe web artifact")
        if not path.parts or path.parts[0] not in ("web", "downloads"):
            raise SystemExit("Unexpected artifact root")
    archive.extractall(sys.argv[2], members=members, filter="data")
PY
# Unprovisioned builds deliberately omit the signed directory; ingress returns 503.
# Release CI verifies directory trust before publishing a provisioned network.
[[ -f $candidate/web/index.html ]] || exit 1
chmod -R a+rX "$candidate"
destination="$root/releases/$revision-$expected"
if [[ ! -d $destination ]]; then mv -- "$candidate" "$destination"; fi
touch -- "$destination"
# Serialize the release switch with the timer, which may still be reading the old current.
if [[ -d "$root/directory" ]]; then
    exec 8<"$root/directory"
    flock 8
    /opt/windshare-network/network-sign mirror --local-only \
        --bundle "$destination/web/.well-known/windshare-network" --out "$root/directory/windshare-network.json"
fi
link="$root/.current.$$"
ln -s -- "$destination" "$link"
mv -Tf -- "$link" "$root/current"
pruned=$(python3 - "$root/releases" "$destination" "$retain_releases" <<'PY'
import pathlib, re, shutil, sys

releases_dir = pathlib.Path(sys.argv[1]).resolve()
active = pathlib.Path(sys.argv[2]).resolve()
retain = int(sys.argv[3])
release_dir_pattern = re.compile(r"^[0-9a-f]{40}-[0-9a-f]{64}$")

candidates = [
    entry
    for entry in releases_dir.iterdir()
    if not entry.is_symlink()
    and entry.is_dir()
    and release_dir_pattern.match(entry.name)
    and entry.resolve() != active
]
candidates.sort(key=lambda entry: entry.stat().st_mtime, reverse=True)
removed = 0
for stale in candidates[max(retain - 1, 0):]:
    shutil.rmtree(stale)
    removed += 1
print(removed)
PY
)
printf 'event=web_release_installed revision=%s sha256=%s pruned=%s\n' "$revision" "$expected" "$pruned"
