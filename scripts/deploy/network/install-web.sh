#!/usr/bin/env bash
set -euo pipefail

artifact=${1:?artifact path required}
expected=${2:?SHA-256 required}
revision=${3:?40-character commit required}
[[ $expected =~ ^[0-9a-f]{64}$ && $revision =~ ^[0-9a-f]{40}$ ]] || exit 2
[[ $(id -u) == 0 && -f $artifact && ! -L $artifact ]] || exit 2
readonly root=/srv/windshare
readonly retain_releases=3
readonly account=windshare-network
install -d -m 0755 "$root/releases" "$root/downloads"
exec 9>/run/lock/windshare-web.lock
flock 9
actual=$(sha256sum "$artifact")
[[ ${actual%% *} == "$expected" ]] || { echo 'Web artifact checksum mismatch' >&2; exit 1; }
candidate=""
downloads_stage=""
link=""
trap 'rm -rf -- ${candidate:+"$candidate"} ${downloads_stage:+"$downloads_stage"} ${link:+"$link"}' EXIT
candidate=$(mktemp -d "$root/releases/.stage.XXXXXXXX")
downloads_stage=$(mktemp -d "$root/.downloads-stage.XXXXXXXX")

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
        if path.parts[0] == "downloads" and any(part.startswith(".") for part in path.parts[1:]):
            raise SystemExit("Unexpected hidden download entry")
    archive.extractall(sys.argv[2], members=members, filter="data")
PY
# Unprovisioned builds deliberately omit the signed directory; ingress returns 503.
# Release CI verifies directory trust before publishing a provisioned network.
[[ -f $candidate/web/index.html ]] || exit 1
chmod -R a+rX "$candidate"
destination="$root/releases/$revision-$expected"
if [[ ! -d $destination ]]; then mv -- "$candidate" "$destination"; fi
touch -- "$destination"

# Publish versioned downloads for the active and retained releases before switching
# current so newly loaded pages and already-open tabs both resolve their routes.
python3 - "$root/releases" "$destination" "$root/downloads" "$downloads_stage" "$retain_releases" <<'PY'
import errno, os, pathlib, re, shutil, sys

RELEASE_DIR_PATTERN = re.compile(r"^[0-9a-f]{40}-[0-9a-f]{64}$")

def link_or_copy(src: pathlib.Path, dst: pathlib.Path) -> None:
    try:
        os.link(src, dst)
    except OSError as exc:
        if exc.errno != errno.EXDEV:
            raise
        shutil.copy2(src, dst)

def populate_tree(src_dir: pathlib.Path, dst_dir: pathlib.Path) -> None:
    for src_entry in src_dir.iterdir():
        if src_entry.is_symlink() or src_entry.name.startswith("."):
            continue
        dst_entry = dst_dir / src_entry.name
        if src_entry.is_dir():
            dst_entry.mkdir(mode=0o755)
            os.chmod(dst_entry, 0o755)
            populate_tree(src_entry, dst_entry)
        elif src_entry.is_file():
            link_or_copy(src_entry, dst_entry)

def sync_tree(src_dir: pathlib.Path, dst_dir: pathlib.Path, stage_root: pathlib.Path) -> None:
    if dst_dir.is_symlink() or (dst_dir.exists() and not dst_dir.is_dir()):
        dst_dir.unlink()
    if not dst_dir.exists():
        staged = stage_root / f"d.{os.getpid()}.{dst_dir.name}"
        if staged.exists():
            shutil.rmtree(staged)
        staged.mkdir(mode=0o755)
        os.chmod(staged, 0o755)
        populate_tree(src_dir, staged)
        os.rename(staged, dst_dir)
        return
    os.chmod(dst_dir, 0o755)
    expected_names = set()
    for src_entry in src_dir.iterdir():
        if src_entry.is_symlink() or src_entry.name.startswith("."):
            continue
        expected_names.add(src_entry.name)
        dst_entry = dst_dir / src_entry.name
        if src_entry.is_dir():
            sync_tree(src_entry, dst_entry, stage_root)
        elif src_entry.is_file():
            if dst_entry.is_dir() and not dst_entry.is_symlink():
                shutil.rmtree(dst_entry)
            elif dst_entry.exists() and not dst_entry.is_symlink() and os.path.samefile(src_entry, dst_entry):
                continue
            staged_file = stage_root / f"f.{os.getpid()}.{src_entry.name}"
            if staged_file.exists() or staged_file.is_symlink():
                staged_file.unlink()
            link_or_copy(src_entry, staged_file)
            os.replace(staged_file, dst_entry)
    for existing in list(dst_dir.iterdir()):
        if existing.name not in expected_names:
            if existing.is_dir() and not existing.is_symlink():
                shutil.rmtree(existing)
            else:
                existing.unlink()

releases_dir = pathlib.Path(sys.argv[1]).resolve()
active = pathlib.Path(sys.argv[2]).resolve()
downloads_dir = pathlib.Path(sys.argv[3]).resolve()
stage_root = pathlib.Path(sys.argv[4]).resolve()
retain = int(sys.argv[5])

older = [
    entry
    for entry in releases_dir.iterdir()
    if not entry.is_symlink()
    and entry.is_dir()
    and RELEASE_DIR_PATTERN.match(entry.name)
    and entry.resolve() != active
]
older.sort(key=lambda entry: entry.stat().st_mtime_ns, reverse=True)
retained = [active, *older[:max(retain - 1, 0)]]

desired: dict[str, pathlib.Path] = {}
for release in retained:
    release_downloads = release / "downloads"
    if release_downloads.is_symlink() or not release_downloads.is_dir():
        continue
    for version_dir in release_downloads.iterdir():
        if (
            version_dir.is_symlink()
            or not version_dir.is_dir()
            or version_dir.name.startswith(".")
            or version_dir.name in desired
        ):
            continue
        desired[version_dir.name] = version_dir

for version, src_dir in desired.items():
    sync_tree(src_dir, downloads_dir / version, stage_root)
PY

# Serialize the release switch with the timer, which may still be reading the old current.
if [[ -L "$root/directory" ]]; then
    echo 'Directory mirror path must not be a symlink' >&2
    exit 1
fi
if [[ -d "$root/directory" ]]; then
    exec 8<"$root/directory"
    flock 8
    # Drop root privileges and close both host lock FDs before touching service-owned mirror state.
    runuser -u "$account" -- /opt/windshare-network/network-sign mirror --local-only \
        --bundle "$destination/web/.well-known/windshare-network" \
        --out "$root/directory/windshare-network.json" 8<&- 9>&-
fi
link="$root/.current.$$"
ln -s -- "$destination" "$link"
mv -Tf -- "$link" "$root/current"
exec 8<&-

prune_summary=$(python3 - "$root/releases" "$destination" "$root/downloads" "$retain_releases" <<'PY'
import pathlib, re, shutil, sys

RELEASE_DIR_PATTERN = re.compile(r"^[0-9a-f]{40}-[0-9a-f]{64}$")

releases_dir = pathlib.Path(sys.argv[1]).resolve()
active = pathlib.Path(sys.argv[2]).resolve()
downloads_dir = pathlib.Path(sys.argv[3]).resolve()
retain = int(sys.argv[4])

older = [
    entry
    for entry in releases_dir.iterdir()
    if not entry.is_symlink()
    and entry.is_dir()
    and RELEASE_DIR_PATTERN.match(entry.name)
    and entry.resolve() != active
]
older.sort(key=lambda entry: entry.stat().st_mtime_ns, reverse=True)
keep_count = max(retain - 1, 0)
retained = [active, *older[:keep_count]]
removed_releases = 0
for stale in older[keep_count:]:
    shutil.rmtree(stale)
    removed_releases += 1

keep_versions: set[str] = set()
for release in retained:
    release_downloads = release / "downloads"
    if release_downloads.is_symlink() or not release_downloads.is_dir():
        continue
    for version_dir in release_downloads.iterdir():
        if not version_dir.is_symlink() and version_dir.is_dir() and not version_dir.name.startswith("."):
            keep_versions.add(version_dir.name)

removed_downloads = 0
for existing in list(downloads_dir.iterdir()):
    if existing.name in keep_versions and not existing.is_symlink() and existing.is_dir():
        continue
    if existing.is_dir() and not existing.is_symlink():
        shutil.rmtree(existing)
    else:
        existing.unlink()
    removed_downloads += 1

print(removed_releases, len(keep_versions), removed_downloads)
PY
)
read -r pruned downloads_retained downloads_pruned <<< "$prune_summary"
printf 'event=web_release_installed revision=%s sha256=%s pruned=%s downloads_retained=%s downloads_pruned=%s\n' \
    "$revision" "$expected" "$pruned" "$downloads_retained" "$downloads_pruned"
