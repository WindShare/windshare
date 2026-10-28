"""Native product transfer across SSH-owned endpoints.

SSH carries signaling; auto/relay-only may also carry application-relay content.

Run through the study terminal after the browser matrix, with paramiko installed.
Public credentials are read from the existing named SSH MCP profile, never saved.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import select
import shlex
import socket
import subprocess
import sys
import threading
import time
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory
sys.path.insert(0, str(ROOT / '.tmp/throughput/python-deps'))
import paramiko

def connect(endpoint):
    client = paramiko.SSHClient()
    client.load_system_host_keys()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    if endpoint == 'lan':
        lab = json.loads((ROOT / '.tmp/webrtc-preflight-20261023/lab.json').read_text())
        user, host = lab['WS_LAB_SSH_TARGET'].split('@')
        options = dict(hostname=host, username=user, port=int(lab['WS_LAB_SSH_PORT']), key_filename=lab['WS_LAB_SSH_KEY'])
    else:
        document = json.loads((ROOT / '.mcp.json').read_text(encoding='utf-8-sig'))
        values = [arg for server in document['mcpServers'].values() for arg in server.get('args', []) if 'name=' + endpoint + ',' in arg]
        if len(values) != 1: raise RuntimeError('Expected one configured SSH study endpoint')
        profile = dict(part.split('=', 1) for part in values[0].split(','))
        options = dict(hostname=profile['host'], username=profile.get('username', 'root'), port=int(profile.get('port', 22)))
        if 'password' in profile: options['password'] = profile['password']
        if 'privateKey' in profile: options['key_filename'] = profile['privateKey']
    client.connect(**options, timeout=15, auth_timeout=15, look_for_keys=False, allow_agent=False)
    return client

class Process:
    def __init__(self, argv, directory, environment=None, remote=None):
        self.output, self.errors = bytearray(), bytearray()
        self.remote = remote
        if remote:
            # Closing the owning channel kills only this invocation's process group.
            # Poll stdin without a buffered-reader daemon: interpreter shutdown
            # must preserve the child's exit status after a successful transfer.
            script = "import os,select,signal,subprocess,sys\np=subprocess.Popen(" + repr(argv) + ",cwd=" + repr(str(directory)) + ",env=dict(os.environ,**" + repr(environment or {}) + "),start_new_session=True)\nwhile p.poll() is None:\n if select.select([0],[],[],0.1)[0] and not os.read(0,1):\n  try: os.killpg(p.pid,signal.SIGTERM)\n  except ProcessLookupError: pass\n  break\nsys.exit(p.wait())"
            self.input, self.stdout, self.stderr = remote.exec_command('python3 -u -c ' + shlex.quote(script))
            self.channel = self.stdout.channel
        else:
            self.process = subprocess.Popen(argv, cwd=directory, env=dict(os.environ, **(environment or {})), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, creationflags=subprocess.CREATE_NO_WINDOW)
            self.input, self.stdout, self.stderr = self.process.stdin, self.process.stdout, self.process.stderr
        self.threads = [threading.Thread(target=self._read, args=(stream, destination), daemon=True) for stream, destination in [(self.stdout, self.output), (self.stderr, self.errors)]]
        for thread in self.threads: thread.start()

    def _read(self, stream, destination):
        while True:
            chunk = stream.read(1)
            if not chunk: break
            destination.extend(chunk)

    def poll(self):
        if self.remote: return self.channel.recv_exit_status() if self.channel.exit_status_ready() else None
        return self.process.poll()

    def text(self): return self.output.decode(errors='replace')

    def close(self):
        if self.poll() is None:
            if self.remote: self.channel.shutdown_write()
            else: self.process.terminate()
            deadline = time.monotonic() + 8
            while self.poll() is None and time.monotonic() < deadline: time.sleep(.05)
            if self.poll() is None:
                if self.remote: self.channel.close()
                else: self.process.kill()
        for thread in self.threads: thread.join(timeout=1)

def until(predicate, process=None, seconds=25):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = predicate()
        if value: return value
        if process and process.poll() is not None: raise RuntimeError('Process exited: ' + process.errors.decode(errors='replace')[-2000:])
        time.sleep(.02)
    raise TimeoutError('Readiness deadline exceeded')

def forward(transport, local_port):
    remote_port = transport.request_port_forward('127.0.0.1', local_port)
    if remote_port != local_port: raise RuntimeError('Signaling tunnel port mismatch')
    def relay(channel):
        target = None
        try:
            target = socket.create_connection(('127.0.0.1', local_port))
            while True:
                ready, _, _ = select.select([channel, target], [], [], 2)
                for origin in ready:
                    data = origin.recv(65536)
                    if not data: return
                    (target if origin is channel else channel).sendall(data)
        except (OSError, EOFError):
            pass
        finally:
            channel.close()
            if target: target.close()
    def accept():
        while transport.is_active():
            try: channel = transport.accept(1)
            except (OSError, EOFError): return
            if channel: threading.Thread(target=relay, args=(channel,), daemon=True).start()
    threading.Thread(target=accept, daemon=True).start()

def remote_command(client, script):
    _, stdout, stderr = client.exec_command('python3 -c ' + shlex.quote(script), timeout=30)
    text = stdout.read().decode()
    if stdout.channel.recv_exit_status(): raise RuntimeError(stderr.read().decode())
    return text

def windows_udp_counters():
    import ctypes

    class UDPStatistics(ctypes.Structure):
        _fields_ = [(name, ctypes.c_uint32) for name in
                    ("InDatagrams", "NoPorts", "InErrors", "OutDatagrams", "NumAddrs")]

    result = {}
    for name, family in [("ipv4", socket.AF_INET), ("ipv6", socket.AF_INET6)]:
        value = UDPStatistics()
        status = ctypes.windll.iphlpapi.GetUdpStatisticsEx(ctypes.byref(value), family)
        result[name] = {key: getattr(value, key) for key, _ in value._fields_} if status == 0 else {"error": status}
    return result


def linux_udp_counters(client):
    script = """import json
from pathlib import Path
result = {}
lines = Path('/proc/net/snmp').read_text().splitlines()
for index in range(0, len(lines)-1, 2):
    names, values = lines[index].split(), lines[index+1].split()
    if names[0] == 'Udp:':
        result['ipv4'] = dict(zip(names[1:], map(int, values[1:])))
path = Path('/proc/net/snmp6')
if path.exists():
    result['ipv6'] = {name[4:]: int(value) for name, value in
                      (line.split() for line in path.read_text().splitlines()) if name.startswith('Udp6')}
print(json.dumps(result))
"""
    return json.loads(remote_command(client, script))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('endpoint')
    parser.add_argument('--build', default='webrtc-adaptive-20261023-final')
    parser.add_argument('--size-mib', type=float, default=256)
    parser.add_argument('--repeats', type=int, default=3)
    parser.add_argument('--direction', choices=['reverse', 'forward'], default='reverse')
    parser.add_argument('--connectivity', choices=['p2p-only', 'auto', 'relay-only'], default='p2p-only')
    parser.add_argument('--deadline-seconds', type=float, default=180)
    parser.add_argument('--block-size', type=int, default=1048576)
    parser.add_argument('--fault', choices=['none', 'drop-large-once'], default='none')
    parser.add_argument('--receive-buffer', type=int, default=0)
    parser.add_argument('--remote-interface', default='')
    args = parser.parse_args()
    if args.deadline_seconds <= 0 or args.block_size <= 0:
        parser.error('Deadline and block size must be positive')
    build = experiment_directory(ROOT, args.build)
    windows_binary = Path(os.environ.get('WS_STUDY_WINDOWS_BINARY', build / 'binaries/wind.exe'))
    study = build / ('native-' + args.endpoint + '-' + args.direction + '-' + str(time.time_ns()))
    study.mkdir()
    size = round(args.size_mib * 1048576)
    fixture = (build / 'fixture.bin').read_bytes()[:size]
    assert len(fixture) == size
    (study / 'probe.bin').write_bytes(fixture)
    (study / 'config.json').write_text('{"profiles":[],"active_relays":[]}')
    report = dict(endpoint=args.endpoint, direction=args.direction, size=size, sha256=hashlib.sha256(fixture).hexdigest(), rounds=[], connectivity=args.connectivity, deadlineSeconds=args.deadline_seconds, blockSize=args.block_size, scope='Complete native CLI workflow. Direct WebRTC content bypasses SSH; application-relay content in auto/relay-only crosses the SSH signaling tunnel. Timing begins at receiver process launch; source preparation and post-publication hash excluded.')
    report['windowsSHA256'] = hashlib.sha256(windows_binary.read_bytes()).hexdigest()
    report['diagnosticOverlay'] = 'WS_STUDY_WINDOWS_BINARY' in os.environ or (build / 'identity.json').exists()
    report['fault'] = args.fault
    report['remoteInterface'] = args.remote_interface
    report['receiveBufferBytes'] = args.receive_buffer
    def save(): (study / 'report.json').write_text(json.dumps(report, indent=2))
    save()
    processes = []
    client = connect(args.endpoint)
    remote = remote_command(client, "import tempfile; print(tempfile.mkdtemp(prefix='windshare-throughput-'))").strip()
    report['remoteDirectory'] = remote
    try:
        with client.open_sftp() as sftp:
            sftp.put(str(build / 'binaries/wind-linux'), remote + '/wind'); sftp.chmod(remote + '/wind', 0o700)
            sftp.put(str(study / 'probe.bin'), remote + '/probe.bin')
            sftp.put(str(study / 'config.json'), remote + '/config.json')
        report['linuxSHA256'] = remote_command(client, "import hashlib; print(hashlib.sha256(open(" + repr(remote + '/wind') + ",'rb').read()).hexdigest())").strip()
        assert report['linuxSHA256'] == hashlib.sha256((build / 'binaries/wind-linux').read_bytes()).hexdigest()
        clock_samples = []
        for _ in range(3):
            before = time.time() * 1000
            remote_ms = float(remote_command(client, 'import time; print(time.time()*1000)'))
            after = time.time() * 1000
            clock_samples.append({'offsetMs': remote_ms - (before + after) / 2, 'roundTripMs': after - before})
        report['remoteClock'] = min(clock_samples, key=lambda sample: sample['roundTripMs'])
        relay = Process([str(build / 'binaries/relay-native.exe'), '--listen', '127.0.0.1:0', '--database', str(study / 'relay.sqlite'), '--origins', 'http://127.0.0.1:9999', '--stun-udp', '127.0.0.1:0', '--stun-admin', '127.0.0.1:0'], ROOT)
        processes.append(relay)
        def relay_ready():
            for line in relay.text().splitlines():
                try:
                    item = json.loads(line)
                    if item.get('event') == 'listening': return item['endpoint']
                except ValueError: pass
        endpoint = until(relay_ready, relay)
        port = urlparse(endpoint).port
        forward(client.get_transport(), port)
        relay_url = 'ws://127.0.0.1:' + str(port)
        for repeat in range(args.repeats):
            row = dict(repetition=repeat + 1, startedMs=time.time() * 1000)
            row['udpCountersBefore'] = {'windows': windows_udp_counters(), 'linux': linux_udp_counters(client)}
            report['rounds'].append(row); save()
            reverse = args.direction == 'reverse'
            sender_root, receiver_root = (remote, str(study)) if reverse else (str(study), remote)
            sender_binary = remote + '/wind' if reverse else str(windows_binary)
            receiver_binary = str(windows_binary) if reverse else remote + '/wind'
            sender = Process([sender_binary, 'share', sender_root + '/probe.bin', '--relay', relay_url, '--front-url', 'http://127.0.0.1:9999', '--block-size', str(args.block_size), '--split-key', '--trace', sender_root + f'/sender-{repeat}.ndjson'], sender_root, {'WINDSHARE_CONFIG': sender_root + '/config.json', 'WS_WAN_FAULT': args.fault,
                'WS_WAN_RCVBUF': str(args.receive_buffer), **({'WS_WAN_INTERFACE': args.remote_interface} if reverse else {})}, client if reverse else None)
            processes.append(sender)
            until(lambda: 'Key: ' in sender.text() and 'Bare link: ' in sender.text(), sender)
            link = next(line[11:] for line in sender.text().splitlines() if line.startswith('Bare link: '))
            key = next(line[5:] for line in sender.text().splitlines() if line.startswith('Key: '))
            output = receiver_root + f'/output-{repeat}'
            if reverse: Path(output).mkdir()
            else: remote_command(client, 'import os; os.mkdir(' + repr(output) + ')')
            started = time.monotonic()
            row['receiverStartedMs'] = time.time() * 1000
            receiver = Process([receiver_binary, 'get', link, '--key', key, '-o', output, '--connectivity', args.connectivity, '--wait-timeout', '35s', '--trace', receiver_root + f'/receiver-{repeat}.ndjson'], receiver_root, {'WINDSHARE_CONFIG': receiver_root + '/config.json', 'WS_WAN_RCVBUF': str(args.receive_buffer),
                **({'WS_WAN_INTERFACE': args.remote_interface} if not reverse else {})}, None if reverse else client)
            processes.append(receiver)
            deadline = started + args.deadline_seconds
            while receiver.poll() is None and time.monotonic() < deadline: time.sleep(.02)
            row['completionMs'] = (time.monotonic() - started) * 1000
            row['exitCode'] = receiver.poll()
            row['deadlineExceeded'] = row['exitCode'] is None
            row['stdout'] = receiver.text(); row['stderr'] = receiver.errors.decode(errors='replace')[-4000:]
            receiver.close(); sender.close()
            row['udpCountersAfter'] = {'windows': windows_udp_counters(), 'linux': linux_udp_counters(client)}
            (study / f'receiver-{repeat}.stderr').write_bytes(receiver.errors)
            (study / f'sender-{repeat}.stderr').write_bytes(sender.errors)
            with client.open_sftp() as sftp:
                for kind in ['sender', 'receiver']:
                    if (kind == 'sender') == reverse:
                        sftp.get(remote + f'/{kind}-{repeat}.ndjson', str(study / f'{kind}-{repeat}.ndjson'))
                for name in sftp.listdir(remote):
                    if name.startswith('research-'):
                        sftp.get(remote + '/' + name, str(study / name))
            if row['exitCode'] != 0: raise RuntimeError('Native transfer failed: ' + row['stderr'])
            if reverse: received = hashlib.sha256((Path(output) / 'probe.bin').read_bytes()).hexdigest()
            else: received = remote_command(client, "import hashlib; print(hashlib.sha256(open(" + repr(output + '/probe.bin') + ",'rb').read()).hexdigest())").strip()
            assert received == report['sha256'], 'Output hash mismatch'
            row['sha256'] = received; row['MBpsIncludingStartup'] = size / row['completionMs'] / 1000
            save(); print(json.dumps({k:v for k,v in row.items() if k not in ['stdout','stderr']}), flush=True)
        report['status'] = 'passed'
    except Exception as error:
        report['status'] = 'failed'; report['error'] = str(error)
        raise
    finally:
        for process in reversed(processes): process.close()
        # This directory was returned by mkdtemp above and owns every remote file.
        remote_command(client, 'import pathlib,shutil; p=pathlib.Path(' + repr(remote) + "); assert p.parent==pathlib.Path('/tmp') and p.name.startswith('windshare-throughput-'); shutil.rmtree(p)")
        report['remoteCleanup'] = 'complete'; save(); client.close()
        print(str(study / 'report.json'), flush=True)

if __name__ == '__main__': main()
