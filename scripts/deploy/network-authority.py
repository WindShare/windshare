#!/usr/bin/env python3
"""Deploy the account authority; keep its root seed on the operator's machine."""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parents[2]
CRATE = ROOT / "relay-rust/crates/network-authority"


def run(args, **kwargs):
    subprocess.run(args, cwd=CRATE, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-host", required=True)
    parser.add_argument("--origin-host", default="authority.windshare.cc")
    parser.add_argument("--zone", default="windshare.cc")
    parser.add_argument("--trusted-proxy-ips", default="")
    parser.add_argument("--allowed-origins", required=True)
    parser.add_argument("--trust", type=Path, required=True)
    parser.add_argument("--online-seed", type=Path, required=True)
    parser.add_argument("--initialize", action="store_true")
    args = parser.parse_args()
    host = args.api_host
    if not host.startswith("api.") or urlparse("https://" + host).netloc != host or any(c in host for c in "/:?#"):
        parser.error("--api-host must be the api subdomain of your public domain")
    trust = json.loads(args.trust.read_text(encoding="utf-8"))
    online = args.online_seed.read_text(encoding="utf-8").strip()
    config = (CRATE / "wrangler.toml").read_text(encoding="utf-8")
    origin = args.origin_host
    if urlparse("https://" + origin).netloc != origin or any(c in origin for c in "/:?#") or not origin.endswith("." + args.zone):
        parser.error("--origin-host must belong to the Cloudflare zone")
    # The filed domain keeps split DNS; its api host is a SaaS custom hostname,
    # while only the authority origin is a Worker custom domain on Cloudflare.
    config += "\n[[routes]]\npattern = " + json.dumps(origin) + "\ncustom_domain = true\n"
    config += "\n[[routes]]\npattern = " + json.dumps(host + "/*") + "\nzone_name = " + json.dumps(args.zone) + "\n"
    config += "\n[vars]\nAUTHORITY_ROOT = " + json.dumps(trust["root"])
    config += "\nAUTHORITY_CERTIFICATE = " + json.dumps(trust["certificate"])
    origins = ",".join(dict.fromkeys(["https://" + host, *args.allowed_origins.split(",")]))
    config += "\nALLOWED_ORIGINS = " + json.dumps(origins) + "\n"
    config += "\nTRUSTED_PROXY_IPS = " + json.dumps(args.trusted_proxy_ips) + "\n"
    # A config beside the template preserves relative build paths. Only the online
    # seed crosses deployment; root.seed is never read by this helper.
    generated = CRATE / "wrangler.generated.toml"
    generated.write_text(config, encoding="utf-8")
    runner = "npx.cmd" if os.name == "nt" else "npx"
    command = [runner, "wrangler", "--config", str(generated)]
    values = {"AUTHORITY_SIGNING_SEED": online}
    initialization = secrets.token_urlsafe(32) if args.initialize else None
    if initialization:
        values["INITIALIZATION_TOKEN"] = initialization
    with tempfile.TemporaryDirectory(prefix="windshare-authority-") as temporary:
        path = Path(temporary) / "secrets.json"
        path.write_text(json.dumps(values), encoding="utf-8")
        path.chmod(0o600)
        # A generation is one deployment: updating secrets first would expose the
        # new signing seed with the previous certificate to live requests.
        run(command + ["deploy", "--secrets-file", str(path)])
    if initialization:
        print(f"Initialize administrator: https://{host}/console#initialize={initialization}")


if __name__ == "__main__":
    main()
