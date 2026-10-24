"""Small datagram/TCP reachability check; does not alter host or cloud firewalls."""
import importlib.util
import json
import os
from pathlib import Path
import secrets
import socket
import time

spec = importlib.util.spec_from_file_location('cross_host', Path(__file__).with_name('cross-host.py'))
host = importlib.util.module_from_spec(spec)
spec.loader.exec_module(host)

server_source = '''import json,select,socket
udp=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); udp.bind(('0.0.0.0',3478))
tcp=socket.socket(); tcp.bind(('0.0.0.0',3478)); tcp.listen()
print(json.dumps({'ready':True}),flush=True)
while True:
 for current in select.select([udp,tcp],[],[])[0]:
  if current is udp:
   data,address=udp.recvfrom(1024); print(json.dumps({'protocol':'udp','bytes':len(data)}),flush=True); udp.sendto(data,address)
  else:
   client,address=tcp.accept(); client.settimeout(5)
   try: client.sendall(client.recv(1024)); print(json.dumps({'protocol':'tcp'}),flush=True)
   finally: client.close()
'''
public_host = os.environ['WS_STUDY_PUBLIC_HOST']
client = host.connect(os.environ['WS_STUDY_SSH_PROFILE'])
server = None
report = {'endpoint': public_host, 'port': 3478, 'results': [], 'scope': 'Temporary echo listeners; no firewall or installed service changes.'}
try:
    report['firewall'] = host.remote_command(client, "import subprocess; p=subprocess.run(['ufw','status'],capture_output=True,text=True); print(p.stdout); print(p.stderr)")
    server = host.Process(['python3', '-u', '-c', server_source], '/tmp', remote=client)
    host.until(lambda: '\n' in server.text(), server)
    for protocol, kind in [('udp', socket.SOCK_DGRAM), ('tcp', socket.SOCK_STREAM)]:
        row = {'protocol': protocol}
        with socket.socket(socket.AF_INET, kind) as probe:
            probe.settimeout(3)
            payload = secrets.token_bytes(32)
            try:
                probe.connect((report['endpoint'], report['port']))
                probe.sendall(payload)
                row['echoMatched'] = probe.recv(1024) == payload
            except OSError as error: row['error'] = str(error)
        report['results'].append(row)
finally:
    if server:
        server.close()
        report['serverEvents'] = server.text()
        report['serverErrors'] = server.errors.decode(errors='replace')
    client.close()
    report['cleanup'] = 'complete'
    path = host.ROOT / '.tmp/throughput' / ('public-reachability-' + str(time.time_ns()) + '.json')
    path.write_text(json.dumps(report, indent=2))
    print(json.dumps(report, indent=2))
