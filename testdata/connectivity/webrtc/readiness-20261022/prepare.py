import hashlib, io, json, os, pathlib, shutil, subprocess, zipfile

ROOT = pathlib.Path(__file__).resolve().parents[4]
TARGET = ROOT / '.tmp/webrtc-readiness-20261022'
REVISION = '86446a6a'

def digest_tree(directory):
    entries = {}
    for parent, directories, files in os.walk(directory / 'web'):
        directories[:] = [name for name in directories if name != 'node_modules']
        for name in files:
            path = pathlib.Path(parent) / name
            entries[path.relative_to(directory).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    digest = hashlib.sha256(json.dumps(entries, sort_keys=True).encode()).hexdigest()
    return {'sha256': digest, 'files': len(entries)}, entries

def main():
    TARGET.mkdir(parents=True, exist_ok=True)
    manifest_path = TARGET / 'snapshots.json'
    if manifest_path.exists():
        manifest = json.loads(manifest_path.read_text())
        for arm in ('old', 'new'):
            actual, _ = digest_tree(TARGET / arm)
            if actual != manifest[arm]:
                raise RuntimeError('Frozen snapshot changed: ' + arm)
        print(json.dumps(manifest))
        return
    for arm in ('old', 'new'):
        if (TARGET / arm).exists():
            raise RuntimeError('Incomplete snapshot exists: ' + arm)
    archive = subprocess.check_output(['git', 'archive', '--format=zip', REVISION, 'web'], cwd=ROOT)
    with zipfile.ZipFile(io.BytesIO(archive)) as source:
        source.extractall(TARGET / 'old')
    files = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', 'web'], cwd=ROOT).decode().split('\0')
    for relative in files:
        if not relative or not (ROOT / relative).is_file():
            continue
        target = TARGET / 'new' / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / relative, target)
    old, before = digest_tree(TARGET / 'old')
    new, after = digest_tree(TARGET / 'new')
    changes = {path: {'before': before.get(path), 'after': after.get(path)}
               for path in sorted(before.keys() | after.keys()) if before.get(path) != after.get(path)}
    manifest = {'baseRevision': subprocess.check_output(['git', 'rev-parse', REVISION], cwd=ROOT).decode().strip(),
                'old': old, 'new': new, 'changes': changes}
    manifest_path.write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')
    (pathlib.Path(__file__).parent / 'snapshots.json').write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(manifest))

if __name__ == '__main__':
    main()
