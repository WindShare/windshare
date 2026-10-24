"""Freeze current tracked sources and build the exact binaries used by the probe."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[4]
BUILD = ROOT / ".tmp" / "webrtc-preflight-20261023"
SNAPSHOT = BUILD / "snapshot"
FIXTURE_BYTES = 256 * 1024 * 1024
FIXTURE_CHUNK_BYTES = 1024 * 1024
POST_MEASUREMENT_DOCUMENTS = {"docs/plans/webrtc-throughput.md"}

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def git(*args):
    return subprocess.check_output(["git", *args], cwd=ROOT)

def main():
    BUILD.mkdir(parents=True, exist_ok=True)
    marker = BUILD / "sources.json"
    if marker.exists():
        metadata = json.loads(marker.read_text())
        for relative, expected in metadata["files"].items():
            if digest(SNAPSHOT / relative) != expected:
                raise RuntimeError("Frozen source identity changed: " + relative)
            if relative not in POST_MEASUREMENT_DOCUMENTS and digest(ROOT / relative) != expected:
                raise RuntimeError("Working source identity changed: " + relative)
        if digest(BUILD / "fixture.bin") != metadata["fixture"]["sha256"]:
            raise RuntimeError("Fixture identity changed")
        print(json.dumps({"event": "snapshot-verified", "files": len(metadata["files"])}), flush=True)
        build(metadata)
        return
    SNAPSHOT.mkdir()
    files = {}
    for encoded in git("ls-files", "-z").split(b"\0"):
        if not encoded:
            continue
        relative = encoded.decode()
        source = ROOT / relative
        if not source.is_file():
            raise RuntimeError("Tracked source is missing: " + relative)
        target = SNAPSHOT / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
        files[relative] = digest(source)
    fixture = BUILD / "fixture.bin"
    previous = Path(os.environ.get("WS_RESEARCH_FIXTURE", ROOT / ".tmp/webrtc-interleaved-20261022/fixture.bin"))
    if previous.is_file():
        shutil.copyfile(previous, fixture)
    elif "WS_RESEARCH_FIXTURE" in os.environ:
        raise FileNotFoundError(previous)
    else:
        with fixture.open("wb") as output:
            for _ in range(FIXTURE_BYTES // FIXTURE_CHUNK_BYTES):
                output.write(os.urandom(FIXTURE_CHUNK_BYTES))
    if fixture.stat().st_size < FIXTURE_BYTES:
        raise ValueError("Fixture must contain at least 256 MiB")
    metadata = {
        "revision": git("rev-parse", "HEAD").decode().strip(),
        "status": git("status", "--porcelain=v1").decode(),
        "files": files,
        "indexSHA256": digest(ROOT / ".git/index"),
        "fixture": {"path": str(fixture), "bytes": fixture.stat().st_size, "sha256": digest(fixture)},
    }
    marker.write_text(json.dumps(metadata, indent=2) + "\n")
    print(json.dumps({"event": "snapshot-created", "files": len(files), "revision": metadata["revision"]}), flush=True)
    build(metadata)


def build(metadata):
    dependencies = SNAPSHOT / "web/node_modules"
    if not dependencies.exists():
        source = ROOT / "web/node_modules"
        if not source.is_dir():
            raise RuntimeError("Install the pinned web dependencies first")
        if os.name == "nt":
            subprocess.run(["pwsh", "-NoProfile", "-Command",
                "New-Item -ItemType Junction -Path $env:WS_PREFLIGHT_DEPS_LINK -Target $env:WS_PREFLIGHT_DEPS_SOURCE | Out-Null"],
                env=dict(os.environ, WS_PREFLIGHT_DEPS_LINK=str(dependencies), WS_PREFLIGHT_DEPS_SOURCE=str(source)), check=True)
        else:
            dependencies.symlink_to(source, target_is_directory=True)
    binaries = BUILD / "binaries"
    binary_manifest = BUILD / "binaries.json"
    if binary_manifest.exists():
        identity = json.loads(binary_manifest.read_text())
        for name in ["wind.exe", "relay-native.exe"]:
            if digest(binaries / name) != identity[name]:
                raise RuntimeError("Binary identity changed: " + name)
        print(json.dumps({"event": "binaries-verified", "revision": metadata["revision"]}), flush=True)
        return
    binaries.mkdir(exist_ok=True)
    env = dict(os.environ, GOWORK="off", GOTOOLCHAIN="local", CARGO_TARGET_DIR=str(ROOT / "relay-rust/.target"))
    with (BUILD / "build.log").open("w") as log:
        subprocess.run(["go", "build", "-o", str(binaries / "wind.exe"), "./cmd/wind"], cwd=SNAPSHOT, env=env, stdout=log, stderr=log, check=True)
        print(json.dumps({"event": "sender-built"}), flush=True)
        relay = subprocess.run(["rustup", "run", "stable", "cargo", "build", "--locked", "--release",
            "-p", "windshare-relay-native", "--bin", "relay-native", "--message-format=json-render-diagnostics"],
            cwd=SNAPSHOT / "relay-rust", env=env, stdout=subprocess.PIPE, stderr=log, text=True, check=True)
    for line in relay.stdout.splitlines():
        item = json.loads(line)
        if item.get("reason") == "compiler-artifact" and item.get("target", {}).get("name") == "relay-native" and item.get("executable"):
            shutil.copyfile(item["executable"], binaries / "relay-native.exe")
            break
    else:
        raise RuntimeError("No relay executable")
    identity = {name: digest(binaries / name) for name in ["wind.exe", "relay-native.exe"]}
    identity["go"] = subprocess.check_output(["go", "version"], text=True).strip()
    identity["node"] = subprocess.check_output(["node", "--version"], text=True).strip()
    (BUILD / "binaries.json").write_text(json.dumps(identity, indent=2) + "\n")
    print(json.dumps({"event": "build-complete", **identity}), flush=True)

if __name__ == "__main__":
    main()
