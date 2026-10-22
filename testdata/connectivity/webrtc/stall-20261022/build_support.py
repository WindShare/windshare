import hashlib
import io
import json
import os
import pathlib
import shutil
import subprocess
import zipfile

ROOT = pathlib.Path(__file__).resolve().parents[4]
HERE = pathlib.Path(__file__).resolve().parent
BUILD = ROOT / '.tmp/webrtc-stall-20261022'
WEB_REVISION = 'eb1300d7b28494f99587f177b225d9c702dc3e0d'


def upstream(module):
    metadata = json.loads(subprocess.check_output(['go', 'mod', 'download', '-json', module], cwd=ROOT))
    return pathlib.Path(metadata['Dir'])


def module_file(target, replacements):
    target.mkdir(parents=True, exist_ok=True)
    path = target / 'candidate.mod'
    shutil.copyfile(ROOT / 'go.mod', path)
    shutil.copyfile(ROOT / 'go.sum', path.with_suffix('.sum'))
    bindings = {
        'github.com/pion/ice/v4': ROOT / 'third_party/pion/ice',
        'github.com/pion/webrtc/v4': ROOT / 'third_party/pion/webrtc',
        'github.com/pion/turn/v5': ROOT / 'third_party/pion/turn',
        **replacements,
    }
    subprocess.run(['go', 'mod', 'edit', '-modfile='+str(path)] +
                   ['-replace='+name+'='+directory.as_posix() for name, directory in bindings.items()],
                   cwd=ROOT, check=True)
    return path


def prepare_lab():
    BUILD.mkdir(parents=True, exist_ok=True)
    snapshot = BUILD / 'web-snapshot'
    if not snapshot.exists():
        archive = subprocess.check_output(['git', 'archive', '--format=zip', WEB_REVISION, 'web'], cwd=ROOT)
        with zipfile.ZipFile(io.BytesIO(archive)) as source:
            source.extractall(snapshot)
    modules = snapshot / 'web/node_modules'
    if not modules.exists():
        subprocess.run(['node', '--input-type=module', '-e',
                        "import {symlinkSync} from 'node:fs'; symlinkSync(process.argv[1],process.argv[2],'junction')",
                        str(ROOT / 'web/node_modules'), str(modules)], check=True)
    fixture = BUILD / 'fixture.bin'
    if not fixture.exists():
        previous = ROOT / '.tmp/webrtc-interleaved-20261022/fixture.bin'
        if previous.exists():
            shutil.copyfile(previous, fixture)
        else:
            with fixture.open('wb') as output:
                for _ in range(256):
                    output.write(os.urandom(1024 * 1024))
    relay = BUILD / 'relay-native.exe'
    if not relay.exists():
        subprocess.run(['rustup', 'run', 'stable', 'cargo', 'build', '--manifest-path',
                        str(ROOT / 'relay-rust/Cargo.toml'), '--locked', '--release',
                        '-p', 'windshare-relay-native', '--bin', 'relay-native'], cwd=ROOT, check=True)
        shutil.copyfile(ROOT / 'relay-rust/target/release/relay-native.exe', relay)
    legacy = ROOT / '.tmp/webrtc-plan-revision-20261022/baseline/relay-native.exe'
    if not legacy.exists():
        legacy.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(relay, legacy)
    (BUILD / 'lab-build.json').write_text(json.dumps({
        'webRevision': WEB_REVISION,
        'fixtureSHA256': hashlib.sha256(fixture.read_bytes()).hexdigest(),
    }, indent=2)+'\n')
    return relay
