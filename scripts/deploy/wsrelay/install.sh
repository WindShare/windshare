#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "Usage: sh scripts/deploy/wsrelay/install.sh <public DNS name or IP>" >&2
    exit 2
fi
bundle=$(CDPATH='' cd -- "$(dirname -- "$0")/../../../deploy/wsrelay" && pwd)
instance="$bundle/instance"
mkdir -p "$instance"
python3 - "$1" "$bundle/node.example.json" "$instance" <<'PY'
import ipaddress
import json
from pathlib import Path
import re
import sys

host, template, dest = sys.argv[1:]
try:
    ip = ipaddress.ip_address(host.strip("[]"))
except ValueError:
    host = host.lower().rstrip(".")
    label = r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"
    if len(host) > 253 or not re.fullmatch(rf"{label}(?:\.{label})+", host) or host.rsplit(".", 1)[1].isdigit():
        sys.exit("Enter a public DNS name or a global-unicast IP.")
    cert = "domain_cert"
else:
    if not ip.is_global or ip.is_multicast or ip.is_reserved or ip.is_link_local:
        sys.exit("The certificate requires a publicly routable IP.")
    host = f"[{ip}]" if ip.version == 6 else str(ip)
    cert = "ip_cert"

path = Path(dest) / "node.json"
endpoint = f"wss://{host}/v2/ws"
if path.exists():
    if json.loads(path.read_text()).get("endpoint") != endpoint:
        sys.exit(f"{path} belongs to another endpoint.")
else:
    node = json.loads(Path(template).read_text())
    node["endpoint"] = endpoint
    with path.open("x") as out:
        json.dump(node, out, indent=2)
        out.write("\n")

env_file = Path(dest) / ".env"
sni = host.strip("[]")
env = f"RELAY_HOST={host}\nRELAY_SNI={sni}\nCERT_POLICY={cert}\n"
if env_file.exists():
    if env_file.read_text() != env:
        sys.exit(f"Existing {env_file} differs. Review it before changing hosts.")
else:
    with env_file.open("x") as out:
        out.write(env)
PY
compose() { docker compose --env-file "$instance/.env" -f "$bundle/compose.yaml" "$@"; }
compose up --build -d
python3 - "$instance/node.json" "$(compose ps -q relay)" <<'PY'
import http.client
import json
from pathlib import Path
import subprocess
import sys
import time
from urllib.parse import urlsplit

config = json.loads(Path(sys.argv[1]).read_text())
origin = "https://" + urlsplit(config["endpoint"]).netloc
listener = urlsplit("//" + config["listen"])
deadline = time.monotonic() + 15
while time.monotonic() < deadline:
    conn = http.client.HTTPConnection(listener.hostname, listener.port, timeout=1)
    try:
        conn.request("GET", "/api/session")
        state = json.loads(conn.getresponse().read()).get("error")
    except (OSError, http.client.HTTPException, ValueError):
        time.sleep(0.5)
        continue
    finally:
        conn.close()
    if state == "login_required":
        print(f"Admin console: {origin}")
        break
    if state == "uninitialized":
        logs = subprocess.run(["docker", "logs", sys.argv[2]], stdout=subprocess.PIPE, check=True, text=True)
        for line in reversed(logs.stdout.splitlines()):
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if isinstance(event, dict) and event.get("event") == "node_console_setup" and event["url"].startswith(origin + "/#initialize="):
                print("Set your admin password:", event["url"])
                sys.exit(0)
        sys.exit("The relay needs initialization. Restart the relay and open the setup URL in its logs.")
    sys.exit(f"Unexpected admin session response from {listener.netloc}.")
else:
    sys.exit(f"The relay did not become ready on {listener.netloc}. Check the relay container logs.")
PY
