"""Run the complete browser pipeline through an isolated public TURN service."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import time

spec = importlib.util.spec_from_file_location('cross_host', Path(__file__).with_name('cross-host.py'))
host = importlib.util.module_from_spec(spec)
spec.loader.exec_module(host)
ROOT = host.ROOT
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory
build = experiment_directory(ROOT, os.environ.get('WS_ADAPTIVE_BUILD', 'webrtc-adaptive-20261023-verified'))
directory = build / ('public-turn-' + str(time.time_ns()))
directory.mkdir()
external_config = os.environ.get('WS_STUDY_TURN_CONFIG')
client = None
remote = None
server = None
report = {'scope': 'Windows sender to Linux Chrome OPFS through public TURN. Content eligibility restricted to TURN; production credential delegation and transfer pipeline. Direct gathering disabled only in the study. Fresh temporary service/key; existing services unchanged.', 'provider': 'cloudflare' if external_config else 'isolated-pion', 'runs': []}
def save(): (directory / 'report.json').write_text(json.dumps(report, indent=2))
try:
    if external_config:
        path = Path(external_config)
    else:
        public_host = os.environ['WS_STUDY_PUBLIC_HOST']
        client = host.connect(os.environ['WS_STUDY_SSH_PROFILE'])
        remote = host.remote_command(client, "import tempfile; print(tempfile.mkdtemp(prefix='windshare-throughput-'))").strip()
        with client.open_sftp() as sftp:
            sftp.put(str(build / 'binaries/turn-server-linux'), remote + '/turn-server'); sftp.chmod(remote + '/turn-server', 0o700)
        server = host.Process([remote + '/turn-server', '-public', public_host], remote, remote=client)
        host.until(lambda: '\n' in server.text(), server)
        ready = json.loads(server.text().splitlines()[0])
        configuration = {'accounts': [{'id': 'study', 'provider': 'coturn', 'issuer': 'coturn', 'local_budget_bytes': 0, 'credentials': {'shared_secret': ready['sharedSecret'], 'lifetime_seconds': 60}}], 'profiles': [{'id': 'public-study', 'account': 'study', 'urls': [ready['url'], ready['tcpUrl']]}]}
        path = directory / 'turn-config.json'; path.write_text(json.dumps(configuration))
    lab = json.loads((ROOT / '.tmp/webrtc-preflight-20261023/lab.json').read_text())
    env = dict(os.environ, **lab, WS_STUDY_TURN_CONFIG=str(path), WS_RESEARCH_OUTPUT='opfs', WS_RESEARCH_TRACE='off', WS_ADAPTIVE_ARM='adaptive', WS_ADAPTIVE_FAULT='none', WS_PREFLIGHT_WORKFLOW='file', WS_PREFLIGHT_RECEIVERS='1')
    sizes = json.loads(os.environ.get('WS_STUDY_TURN_SIZES', '[0.125, 8, 64, 0.125, 64]'))
    for index, size in enumerate(sizes):
        label = directory.name + '-' + str(index)
        row = {'label': label, 'sizeMiB': size, 'startedMs': time.time() * 1000}
        report['runs'].append(row); save()
        with (directory / (str(index) + '.log')).open('w') as log:
            result = subprocess.run(['node', '--import', './testdata/connectivity/webrtc/slowdown-20261022/loader.mjs', './testdata/connectivity/webrtc/adaptive-20261023/probe.mjs', 'baseline', str(size), '1', '0', 'none', '1', 'lan'], cwd=ROOT, env=dict(env, WS_RESEARCH_LABEL=label), stdout=log, stderr=log)
        row['exitCode'] = result.returncode
        for candidate in (build / 'runs').glob('*/report.json'):
            value = json.loads(candidate.read_text())
            if value.get('label') != label: continue
            row['report'] = str(candidate.relative_to(ROOT))
            row['status'] = value.get('status'); row['error'] = value.get('error')
            row['rounds'] = [{key: item.get(key) for key in ['size', 'MBps', 'sha256', 'activationToFirstWriteMs', 'activationToCompleteMs', 'admissions', 'dispatches', 'peers']} for item in value['rounds']]
            for item in value['rounds']:
                assert item['dispatches'].get('turn', 0) > 0 and set(item['dispatches']) == {'turn'}, 'Content escaped TURN route constraint'
        save(); print(json.dumps({k:v for k,v in row.items() if k != 'rounds'}), flush=True)
        if result.returncode: raise RuntimeError('Public TURN probe failed; inspect preserved evidence')
    report['status'] = 'passed'
except Exception as error:
    report['status'] = 'failed'; report['error'] = str(error)
    raise
finally:
    if server: server.close()
    if remote:
        host.remote_command(client, 'import pathlib,shutil; p=pathlib.Path(' + repr(remote) + "); assert p.parent==pathlib.Path('/tmp') and p.name.startswith('windshare-throughput-'); shutil.rmtree(p)")
    report['remoteCleanup'] = 'externally-owned-key' if external_config else 'complete'
    save()
    if client: client.close()
    print(str(directory / 'report.json'), flush=True)
