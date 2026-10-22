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
if "--capture-netlog" in sys.argv:
    args.append("--log-net-log=" + str(profile / "netlog.json"))
if "--isolate-external-dns" in sys.argv:
    args.append("--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE localhost, EXCLUDE 127.0.0.1, EXCLUDE " + sender_host + ", EXCLUDE " + receiver_host)
probe_env = os.environ.copy()
buffer_mode = "default"
if "--large-recvbuf" in sys.argv:
    library = profile / "receiver-buffer-control.so"
    source = r"""
#define _GNU_SOURCE
#include <sys/socket.h>
#include <dlfcn.h>
#include <errno.h>
typedef int (*set_option_fn)(int,int,int,const void*,socklen_t);
static set_option_fn real_set_option;
__attribute__((constructor)) static void initialize(void) {
    real_set_option = (set_option_fn)dlsym(RTLD_NEXT,"setsockopt");
}
int setsockopt(int fd,int level,int option,const void *value,socklen_t length) {
    if (!real_set_option) { errno=ENOSYS; return -1; }
    if(level==SOL_SOCKET && option==SO_RCVBUF && length==sizeof(int)) {
        int kind=0; socklen_t size=sizeof(kind);
        if(getsockopt(fd,SOL_SOCKET,SO_TYPE,&kind,&size)==0 && kind==SOCK_DGRAM) {
            /* Only this owned diagnostic browser; no global sysctl mutation. */
            const int bytes=4*1024*1024;
            return real_set_option(fd,level,option,&bytes,sizeof(bytes));
        }
    }
    return real_set_option(fd,level,option,value,length);
}
"""
    subprocess.run(["cc","-shared","-fPIC","-O2","-x","c","-","-o",str(library),"-ldl"],input=source,text=True,check=True,capture_output=True)
    probe_env["LD_PRELOAD"] = str(library)
    buffer_mode = "4MiB-requested"
proc = subprocess.Popen(args, stdout=log, stderr=log, start_new_session=True, env=probe_env)
try:
    def interrupted(signum, frame): raise SystemExit(128+signum)
    signal.signal(signal.SIGTERM,interrupted)
    signal.signal(signal.SIGHUP,interrupted)
    marker = profile/"DevToolsActivePort"
    deadline = time.monotonic()+15
    while not marker.exists():
        if proc.poll() is not None or time.monotonic()>deadline: raise RuntimeError("Chrome startup failed")
        time.sleep(.05)
    print(json.dumps({"event":"ready","externalDNSBlocked":"--isolate-external-dns" in sys.argv,"bufferMode":buffer_mode,"tcpPort":tcp.getsockname()[1],"port":int(marker.read_text().splitlines()[0]),"origin":"http://127.0.0.1:"+str(http.server_port),"pid":proc.pid,"profile":str(profile),"args":args}),flush=True)
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
    if "--capture-netlog" in sys.argv:
        encoded = ""
        repaired_footer = False
        try:
            encoded = (profile / "netlog.json").read_text()
            try: document = json.loads(encoded)
            except json.JSONDecodeError:
                repaired_footer = True
                # Chrome may terminate after its last event but before the array footer.
                document = json.loads(encoded.rstrip().rstrip(",") + "\n]}")
            names = {value:key for key,value in document["constants"]["logEventTypes"].items()}
            flows = {}
            for event in document.get("events", []):
                name = names.get(event["type"], str(event["type"]))
                source = str(event["source"]["id"])
                params = event.get("params", {})
                if source not in flows: flows[source] = {"source":source,"receivedBytes":0,"receiveEvents":0,"hints":[],"dependencies":[],"first":event["time"],"last":event["time"]}
                flow = flows[source]
                flow["last"] = event["time"]
                if name.endswith("BYTES_RECEIVED"):
                    flow["receivedBytes"] += params.get("byte_count", 0)
                    flow["receiveEvents"] += 1
                hints = {key:params[key] for key in ["url","host","host_and_port","address","remote_address","peer_address"] if key in params}
                hint = {"event":name,**hints}
                if hints and hint not in flow["hints"] and len(flow["hints"]) < 12: flow["hints"].append(hint)
                dependency = params.get("source_dependency",{}).get("id")
                if dependency is not None and str(dependency) not in flow["dependencies"]: flow["dependencies"].append(str(dependency))
            retained = sorted(flows.values(),key=lambda row:row["receivedBytes"],reverse=True)[:20]
            related = {key:row for key,row in flows.items() if row["hints"] and any(token in json.dumps(row["hints"]).lower() for token in ["google","gvt","component","optimization","model"])}
            # Socket source IDs alone cannot attribute external bytes to a request.
            linked = {row["source"] for row in retained if any(hint.get("event") == "UDP_CONNECT" for hint in row["hints"])}
            for _ in range(3):
                linked |= {dependency for key in list(linked) for dependency in flows.get(key,{}).get("dependencies",[])}
                linked |= {key for key,row in flows.items() if set(row["dependencies"]) & linked}
            print(json.dumps({"event":"netlog","repairedFooter":repaired_footer,"timeTickOffset":document["constants"].get("timeTickOffset"),"topReceiveFlows":retained,"linkedExternalSources":[flows[key] for key in sorted(linked) if key in flows],"backgroundHints":list(related.values())[:50]}),flush=True)
        except Exception as error:
            print(json.dumps({"event":"netlog","error":str(error),"tail":encoded[-2000:]}),flush=True)
    if profile.resolve().parent != profile_parent.resolve() or not profile.name.startswith("windshare-file-research-"):
        raise RuntimeError("unexpected profile parent")
    shutil.rmtree(profile)
    print(json.dumps({"event":"cleanup","profile":str(profile),"status":"complete"}),flush=True)
