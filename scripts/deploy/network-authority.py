#!/usr/bin/env python3
"""Deploy the network authority with one certified signing generation."""
import argparse
import base64
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[2]
CRATE = ROOT / "relay-rust/crates/network-authority"
WRANGLER = ROOT / "relay-rust/crates/relay-cloudflare/node_modules/wrangler/bin/wrangler.js"
HOST = re.compile(r"(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}\Z")
WORKER = re.compile(r"[a-z0-9][a-z0-9-]{0,62}\Z")


def https_origin(address):
    url = urlsplit(address)
    if url.scheme != "https" or not HOST.fullmatch(url.netloc) or url.query or url.fragment or url.path not in ("", "/"):
        raise ValueError("Expected an HTTPS origin with a DNS hostname")
    return "https://" + url.netloc


def verify_release_authority(release, api, root):
    """Ensure a deployment targeting a network release matches its client-pinned authority."""
    authority = release.get("authority")
    if authority is None:
        return
    if not isinstance(authority, dict):
        raise ValueError("Invalid authority section in network release manifest")
    release_url = authority.get("url")
    release_root = authority.get("root")
    if not isinstance(release_url, str) or not isinstance(release_root, str) or not release_root:
        raise ValueError("Network release authority requires url and root")
    expected_api = https_origin(release_url)
    if api != expected_api:
        raise ValueError(
            f"Deployment API origin {api} does not match network release authority url {expected_api}"
        )
    if root != release_root:
        raise ValueError(
            f"Deployment trust root {root} does not match network release authority root {release_root}"
        )


def prepare_token(path, host):
    if os.environ.get("CI"):
        raise ValueError("Prepare the administrator link on the operator machine")
    if not path.exists():
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            output.write(secrets.token_urlsafe(32))
    token = path.read_text(encoding="utf-8").strip()
    if not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", token):
        raise ValueError("Invalid initialization token file")
    print(f"https://{host}/console#initialize={token}")


def directory_seed(value):
    value = value.strip()
    if value.startswith("-----BEGIN PRIVATE KEY-----"):
        # Go keygen emits the RFC 8410 PKCS#8 form with an absent algorithm parameter.
        pem = value.removeprefix("-----BEGIN PRIVATE KEY-----").removesuffix("-----END PRIVATE KEY-----")
        data = base64.b64decode("".join(pem.split()), validate=True)
        prefix = bytes.fromhex("302e020100300506032b657004220420")
        if len(data) != len(prefix) + 32 or not data.startswith(prefix):
            raise ValueError("Expected an Ed25519 PKCS#8 private key")
        value = base64.urlsafe_b64encode(data[len(prefix):]).decode("ascii").rstrip("=")
    if not re.fullmatch(r"[A-Za-z0-9_-]{43}", value):
        raise ValueError("Expected a 32-byte base64url directory seed")
    return value


def deploy(args):
    host = args.api_host
    api = https_origin("https://" + host)
    if args.prepare_initialization:
        prepare_token(args.prepare_initialization, host)
        return
    worker = args.worker_name
    if not WORKER.fullmatch(worker):
        raise ValueError("Invalid worker name")
    origin = args.origin_host or host
    https_origin("https://" + origin)
    if origin != host and (not args.zone or not HOST.fullmatch(args.zone) or not origin.endswith("." + args.zone)):
        raise ValueError("The SaaS origin must belong to the selected zone")
    trust = json.loads(args.trust.read_text(encoding="utf-8") if args.trust else os.environ["AUTHORITY_TRUST"])
    if args.network_release:
        release = json.loads(args.network_release.read_text(encoding="utf-8"))
        if not isinstance(release, dict):
            raise ValueError("Network release manifest must be a JSON object")
        verify_release_authority(release, api, trust["root"])
    seed = args.online_seed.read_text(encoding="utf-8").strip() if args.online_seed else os.environ["AUTHORITY_SIGNING_SEED"].strip()
    token = args.initialization_token.read_text(encoding="utf-8").strip() if args.initialization_token else os.environ.get("INITIALIZATION_TOKEN", "").strip()
    if not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", token):
        raise ValueError("A persisted initialization token is required")
    origins = [https_origin(v.strip()) for v in args.allowed_origins.split(",") if v.strip()]
    proxies = [str(ipaddress.ip_address(v.strip())) for v in args.trusted_proxy_ips.split(",") if v.strip()]
    config = (CRATE / "wrangler.toml").read_text(encoding="utf-8")
    config = re.sub(r"(?m)^name = .*?$", "name = " + json.dumps(worker), config, count=1)
    config += "\n[[routes]]\npattern = " + json.dumps(origin) + "\ncustom_domain = true\n"
    if origin != host:
        config += "\n[[routes]]\npattern = " + json.dumps(host + "/*") + "\nzone_name = " + json.dumps(args.zone) + "\n"
    values = {"AUTHORITY_ROOT": trust["root"], "AUTHORITY_CERTIFICATE": trust["certificate"], "API_ORIGIN": api,
              "ALLOWED_ORIGINS": ",".join(dict.fromkeys([api, *origins])), "TRUSTED_PROXY_IPS": ",".join(proxies)}
    secrets_to_upload = {"AUTHORITY_SIGNING_SEED": seed, "INITIALIZATION_TOKEN": token}
    maintenance = os.environ.get("FLEET_MAINTENANCE_TOKEN", "").strip()
    if maintenance:
        if not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", maintenance):
            raise ValueError("Invalid fleet maintenance token")
        secrets_to_upload["FLEET_MAINTENANCE_TOKEN"] = maintenance
    mail_url = os.environ.get("EMAIL_WEBHOOK_URL", "").strip()
    mail_token = os.environ.get("EMAIL_WEBHOOK_TOKEN", "").strip()
    if mail_url or mail_token:
        endpoint = urlsplit(mail_url)
        if endpoint.scheme != "https" or not endpoint.hostname or endpoint.username is not None or endpoint.fragment or not mail_token:
            raise ValueError("Email delivery requires an HTTPS EMAIL_WEBHOOK_URL and EMAIL_WEBHOOK_TOKEN")
        values["EMAIL_WEBHOOK_URL"] = mail_url
        secrets_to_upload["EMAIL_WEBHOOK_TOKEN"] = mail_token
    extra_domains = [d.strip().lower() for d in os.environ.get("EXTRA_EMAIL_DOMAINS", "").split(",") if d.strip()]
    for domain in extra_domains:
        if not HOST.fullmatch(domain):
            raise ValueError(f"Invalid EXTRA_EMAIL_DOMAINS entry: {domain}")
    if extra_domains:
        values["EXTRA_EMAIL_DOMAINS"] = ",".join(dict.fromkeys(extra_domains))
    namespace = os.environ["DIRECTORY_KV_ID"].strip()
    if not re.fullmatch(r"[0-9a-f]{32}", namespace):
        raise ValueError("Invalid shared directory KV namespace")
    config += '\n[[kv_namespaces]]\nbinding = "DIRECTORY"\nid = ' + json.dumps(namespace) + '\n'
    values["DIRECTORY_CERTIFICATE"] = json.dumps(json.loads(os.environ["DIRECTORY_CERTIFICATE"]), separators=(",", ":"))
    values["VANTAGE_KEYS"] = json.dumps(json.loads(os.environ.get("VANTAGE_KEYS") or "[]"), separators=(",", ":"))
    secrets_to_upload["DIRECTORY_SIGNING_SEED"] = directory_seed(os.environ["DIRECTORY_SIGNING_SEED"])
    config += "\n[vars]\n" + "".join(f"{k} = {json.dumps(v)}\n" for k, v in values.items())
    # Keep the config beside the template for Worker build paths. Credentials only enter
    # Wrangler's atomic code-and-secrets upload after local signature verification.
    with tempfile.TemporaryDirectory(prefix="windshare-authority-") as private:
        trust_path = Path(private) / "trust.json"
        trust_path.write_text(json.dumps(trust), encoding="utf-8")
        seed_path = Path(private) / "online.seed"
        seed_path.write_text(seed, encoding="utf-8")
        subprocess.run(["cargo", "run", "--quiet", "--locked", "--bin", "network-authority-keys", "--",
                        "verify", str(trust_path), str(seed_path)], cwd=CRATE, check=True)
        agents_path = Path(private) / "vantages.json"
        agents_path.write_text(values["VANTAGE_KEYS"], encoding="utf-8")
        subprocess.run(["cargo", "run", "--quiet", "--locked", "--bin", "network-authority-keys", "--",
                        "verify-vantages", str(agents_path)], cwd=CRATE, check=True)
        cert_path = Path(private) / "directory-certificate.json"
        cert_path.write_text(values["DIRECTORY_CERTIFICATE"], encoding="utf-8")
        directory_key = Path(private) / "directory.seed"
        directory_key.write_text(secrets_to_upload["DIRECTORY_SIGNING_SEED"], encoding="utf-8")
        subprocess.run(["cargo", "run", "--quiet", "--locked", "--bin", "network-authority-keys", "--",
                        "verify-publisher", str(cert_path), str(directory_key)], cwd=CRATE, check=True)
        uploads = Path(private) / "secrets.json"
        uploads.write_text(json.dumps(secrets_to_upload), encoding="utf-8")
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", suffix=".toml", prefix="wrangler.generated.", dir=CRATE, delete=False) as generated:
            generated.write(config)
        try:
            subprocess.run(["node", str(WRANGLER), "deploy", "--config", generated.name, "--secrets-file", str(uploads)], cwd=CRATE, check=True)
        finally:
            Path(generated.name).unlink(missing_ok=True)
    print(f"Network authority deployed at {api}/console")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-host", default=os.environ.get("AUTHORITY_API_HOST"), required=not os.environ.get("AUTHORITY_API_HOST"))
    parser.add_argument("--worker-name", default=os.environ.get("AUTHORITY_WORKER_NAME") or "windshare-network-authority")
    parser.add_argument("--origin-host", default=os.environ.get("AUTHORITY_ORIGIN_HOST"))
    parser.add_argument("--zone", default=os.environ.get("AUTHORITY_ZONE"))
    parser.add_argument("--trusted-proxy-ips", default=os.environ.get("AUTHORITY_TRUSTED_PROXY_IPS", ""))
    parser.add_argument("--allowed-origins", default=os.environ.get("AUTHORITY_ALLOWED_ORIGINS", ""))
    parser.add_argument("--trust", type=Path)
    parser.add_argument("--online-seed", type=Path)
    parser.add_argument("--initialization-token", type=Path)
    parser.add_argument("--prepare-initialization", type=Path)
    parser.add_argument("--network-release", type=Path)
    deploy(parser.parse_args())


if __name__ == "__main__":
    main()
