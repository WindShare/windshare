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
import os
from pathlib import Path
import re
import sys

host, template, dest = sys.argv[1:]
RESERVED_TLDS = frozenset({"localhost", "local", "internal", "invalid", "onion", "arpa"})
if host.startswith("[") or host.endswith("]"):
    if not (host.startswith("[") and host.endswith("]")):
        sys.exit("Enter a public DNS name or a global-unicast IP.")
    try:
        ip = ipaddress.ip_address(host[1:-1])
    except ValueError:
        sys.exit("Enter a public DNS name or a global-unicast IP.")
    if ip.version != 6:
        sys.exit("Enter a public DNS name or a global-unicast IP.")
else:
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        ip = None

if ip is None:
    host = host.lower()
    if host.endswith("."):
        host = host[:-1]
    label = r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"
    if (
        len(host) > 253
        or not re.fullmatch(rf"{label}(?:\.{label})+", host)
        or host.rsplit(".", 1)[1].isdigit()
        or host.rsplit(".", 1)[1] in RESERVED_TLDS
    ):
        sys.exit("Enter a public DNS name or a global-unicast IP.")
    cert = "domain_cert"
else:
    # Reject IPv6 zone IDs (which can smuggle newlines into .env) and IPv4-mapped
    # IPv6 addresses (rejected by ACME IP certificate issuance).
    if (
        getattr(ip, "scope_id", None) is not None
        or getattr(ip, "ipv4_mapped", None) is not None
        or not ip.is_global
        or ip.is_multicast
        or ip.is_reserved
        or ip.is_link_local
    ):
        sys.exit("The certificate requires a publicly routable IP.")
    host = f"[{ip}]" if ip.version == 6 else str(ip)
    cert = "ip_cert"

path = Path(dest) / "node.json"
endpoint = f"wss://{host}/v2/ws"
if path.exists():
    existing = json.loads(path.read_text())
    if existing.get("endpoint") != endpoint:
        sys.exit(f"{path} belongs to another endpoint.")
    for field in ("database", "identity_file"):
        value = existing.get(field)
        if not isinstance(value, str) or not value.startswith("/"):
            sys.exit(f"{path} must set an absolute container path for {field} (for example /state/...).")
else:
    node = json.loads(Path(template).read_text())
    node["endpoint"] = endpoint
    # Keep durable state on the writable /state volume because /config is mounted read-only.
    node["database"] = "/state/relay.sqlite"
    node["identity_file"] = "/state/identity.json"
    with path.open("x") as out:
        json.dump(node, out, indent=2)
        out.write("\n")
# The relay container runs as UID 65532; ensure strict host umasks (0027/0077)
# still permit traversing /config and reading node.json.
os.chmod(dest, 0o755)
os.chmod(path, 0o644)

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
python3 - "$instance/node.json" "$instance/.env" "$bundle/compose.yaml" <<'PY'
import http.client
import json
from pathlib import Path
import subprocess
import sys
import time
from urllib.parse import urlsplit

config_path, env_file, compose_file = sys.argv[1:]
config = json.loads(Path(config_path).read_text())
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
        # Resolve the live container only after /api/session responds so transient
        # startup restarts cannot produce an empty container ID or traceback.
        ps = subprocess.run(
            ["docker", "compose", "--env-file", env_file, "-f", compose_file, "ps", "-q", "relay"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        containers = ps.stdout.strip().splitlines()
        if ps.returncode != 0 or not containers:
            detail = ps.stderr.strip()
            suffix = f" ({detail})" if detail else ""
            sys.exit(f"Could not inspect the relay container for {listener.netloc}.{suffix}")
        # Capture stderr so wsrelay's JSON traces do not pollute terminal output,
        # while still surfacing Docker errors if log retrieval fails.
        logs = subprocess.run(
            ["docker", "logs", containers[0]],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        if logs.returncode != 0:
            detail = logs.stderr.strip()
            suffix = f" ({detail})" if detail else ""
            sys.exit(f"Could not read relay container logs.{suffix}")
        for line in reversed(logs.stdout.splitlines()):
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if (
                isinstance(event, dict)
                and event.get("event") == "node_console_setup"
                and isinstance(event.get("url"), str)
                and event["url"].startswith(origin + "/#initialize=")
            ):
                print("Set your admin password:", event["url"])
                sys.exit(0)
        sys.exit("The relay needs initialization. Restart the relay and open the setup URL in its logs.")
    sys.exit(f"Unexpected admin session response from {listener.netloc}.")
else:
    sys.exit(f"The relay did not become ready on {listener.netloc}. Check the relay container logs.")
PY
