#!/usr/bin/env bash
set -euo pipefail

artifact=${1:?artifact path required}
expected=${2:?SHA-256 required}
revision=${3:?40-character commit required}
[[ $expected =~ ^[0-9a-f]{64}$ && $revision =~ ^[0-9a-f]{40}$ ]] || exit 2
[[ $(id -u) == 0 && -f $artifact && ! -L $artifact ]] || exit 2
readonly root=/srv/windshare
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
link="$root/.current.$$"
ln -s -- "$destination" "$link"
mv -Tf -- "$link" "$root/current"
printf 'event=web_release_installed revision=%s sha256=%s\n' "$revision" "$expected"
