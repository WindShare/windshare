import argparse
import importlib.util
import json
from pathlib import Path
import sys
import time

parser = argparse.ArgumentParser(description="Sample only study-owned Linux UDP sockets")
parser.add_argument("--build", required=True)
parser.add_argument("--ledger", required=True)
args = parser.parse_args()
root = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory
build = experiment_directory(root, args.build)
spec = importlib.util.spec_from_file_location("lab", root / "testdata/connectivity/webrtc/adaptive-20261023/cross-host.py")
lab = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lab)
client = lab.connect("lan")
inventory = lab.remote_command(client, """import json,os,subprocess
from pathlib import Path
result = {'uid': os.getuid(), 'sysctl': {}}
for name in ['rmem_default','rmem_max','wmem_default','wmem_max']:
    path = Path('/proc/sys/net/core') / name
    result['sysctl'][name] = int(path.read_text())
probe = subprocess.run(['sudo','-n','true'],capture_output=True)
result['sudoNoninteractive'] = probe.returncode == 0
print(json.dumps(result))
""")
(build / "receive-buffer-inventory.json").write_text(inventory)
print(inventory, flush=True)
remote = lab.remote_command(client, "import tempfile; print(tempfile.mkdtemp(prefix='windshare-udp-observer-'))").strip()
script = """import json,os,time
from pathlib import Path
stream = open('udp-sockets.ndjson','w',buffering=1)
while True:
    owners = {}
    for entry in Path('/proc').iterdir():
        if not entry.name.isdigit(): continue
        try:
            argv = (entry/'cmdline').read_bytes().split(bytes([0]))
            program = argv[0].decode()
            if not (program.startswith('/tmp/windshare-throughput-') or program.startswith('/tmp/windshare-wan-')) or not program.endswith('/wind'): continue
            role = argv[1].decode()
            for fd in (entry/'fd').iterdir():
                try: target = os.readlink(fd)
                except OSError: continue
                if target.startswith('socket:['):
                    owners[target[8:-1]] = {'pid': int(entry.name), 'role': role, 'directory': str(Path(program).parent)}
        except (OSError,IndexError): continue
    sockets = []
    for family,table in [('ipv4','udp'),('ipv6','udp6')]:
        for line in (Path('/proc/net')/table).read_text().splitlines()[1:]:
            fields = line.split()
            if fields[9] in owners:
                sockets.append(dict(owners[fields[9]], family=family, inode=fields[9],
                     receiveQueueBytes=int(fields[4].split(':')[1],16), drops=int(fields[-1])))
    stream.write(json.dumps({'timeMs':time.time()*1000,'sockets':sockets})+'\\n')
    time.sleep(.5)
"""
process = lab.Process(["python3", "-u", "-c", script], remote, remote=client)
try:
    deadline = time.monotonic() + 3600
    while time.monotonic() < deadline:
        ledger_path = build / args.ledger
        ledger = json.loads(ledger_path.read_text()) if ledger_path.exists() else {}
        if ledger.get("finishedMs"):
            break
        if process.poll() is not None:
            raise RuntimeError(process.errors.decode(errors="replace"))
        time.sleep(1)
finally:
    process.close()
    with client.open_sftp() as sftp:
        sftp.get(remote + "/udp-sockets.ndjson", str(build / "linux-udp-sockets.ndjson"))
    lab.remote_command(client, "import pathlib,shutil; p=pathlib.Path(" + repr(remote) + "); assert p.parent==pathlib.Path('/tmp') and p.name.startswith('windshare-udp-observer-'); shutil.rmtree(p)")
    client.close()
print("Socket observation saved; remote cleanup complete.", flush=True)
