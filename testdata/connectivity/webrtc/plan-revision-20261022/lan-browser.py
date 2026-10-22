import json, os, pathlib, select, shutil, signal, subprocess, sys, tempfile, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import socket, hashlib

# Lab addresses are supplied by the launcher; defaults are documentation-only.
receiver_host = os.environ.get("WS_LAB_RECEIVER_HOST", "192.0.2.105")
sender_host = os.environ.get("WS_LAB_SENDER_HOST", "192.0.2.234")

tcp = socket.socket()
tcp.bind((receiver_host,0))
tcp.listen(2)
tcp.settimeout(1)
tcp_stopping = threading.Event()
def accept_tcp():
    while not tcp_stopping.is_set():
        try: connection, peer = tcp.accept()
        except socket.timeout: continue
        except OSError: break
        try:
            connection.settimeout(30)
            total = 0
            digest = hashlib.sha256()
            start = time.monotonic()
            while True:
                data = connection.recv(1024*1024)
                if not data: break
                total += len(data)
                digest.update(data)
            seconds = time.monotonic()-start
            connection.sendall((json.dumps({"bytes":total,"seconds":seconds,"MBps":total/seconds/1e6,"sha256":digest.hexdigest()})+"\n").encode())
        finally: connection.close()
threading.Thread(target=accept_tcp,daemon=True).start()

profile_parent = pathlib.Path.home() / ".cache"
profile_parent.mkdir(exist_ok=True)
profile = pathlib.Path(tempfile.mkdtemp(prefix="windshare-file-research-", dir=profile_parent))
class Page(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header("Content-Type", "text/html"); self.end_headers()
        self.wfile.write(b"<!doctype html><title>WindShare transport research</title>")
    def log_message(self, *args): pass
http = ThreadingHTTPServer(("127.0.0.1", 0), Page)
threading.Thread(target=http.serve_forever, daemon=True).start()
log = (profile/"chrome.log").open("w")
args = ["google-chrome", "--headless=new", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-features=WebRtcHideLocalIpsWithMdns", "--remote-debugging-port=0", "--user-data-dir="+str(profile), "about:blank"]
proc = subprocess.Popen(args, stdout=log, stderr=log, start_new_session=True)
try:
    def interrupted(signum, frame): raise SystemExit(128+signum)
    signal.signal(signal.SIGTERM,interrupted)
    signal.signal(signal.SIGHUP,interrupted)
    marker = profile/"DevToolsActivePort"
    deadline = time.monotonic()+15
    while not marker.exists():
        if proc.poll() is not None or time.monotonic()>deadline: raise RuntimeError("Chrome startup failed")
        time.sleep(.05)
    print(json.dumps({"event":"ready","tcpPort":tcp.getsockname()[1],"port":int(marker.read_text().splitlines()[0]),"origin":"http://127.0.0.1:"+str(http.server_port),"pid":proc.pid,"profile":str(profile),"args":args}),flush=True)
    deadline=time.monotonic()+1800
    while time.monotonic()<deadline:
        if proc.poll() is not None: break
        if not select.select([sys.stdin],[],[],1)[0]: continue
        line=sys.stdin.readline()
        if not line: break
        cmd=json.loads(line)
        if cmd.get("command")=="stop": break
        if cmd.get("command")=="snapshot":
            snapshots={}
            for protocol,flags in [("udp","-uampn"),("tcp","-ntipn")]:
                snapshots[protocol]=subprocess.run(["ss",flags,"dst",sender_host],capture_output=True,text=True).stdout
            print(json.dumps({"event":"snapshot","id":cmd.get("id"),"sockets":snapshots}),flush=True)
finally:
    tcp_stopping.set()
    tcp.close()
    http.shutdown()
    try: os.killpg(proc.pid,signal.SIGTERM)
    except ProcessLookupError: pass
    try: proc.wait(timeout=5)
    except subprocess.TimeoutExpired: os.killpg(proc.pid,signal.SIGKILL);proc.wait()
    log.close()
    if profile.resolve().parent != profile_parent.resolve() or not profile.name.startswith("windshare-file-research-"):
        raise RuntimeError("unexpected profile parent")
    shutil.rmtree(profile)
    print(json.dumps({"event":"cleanup","profile":str(profile),"status":"complete"}),flush=True)
