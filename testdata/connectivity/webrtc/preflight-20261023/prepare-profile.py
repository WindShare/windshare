"""Build an opt-in Go CPU/memory sampler without editing product sources."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[4]
HERE = Path(__file__).parent
BUILD = ROOT / ".tmp/webrtc-preflight-20261023"
target = BUILD / "binaries-profile"
target.mkdir(exist_ok=True)
overlay = BUILD / "profile-overlay.json"
overlay.write_text(json.dumps({"Replace": {
    str(BUILD / "snapshot/cmd/wind/z_preflight_profile.go"): str(HERE / "native-profile.go.txt")
}}, indent=2))
subprocess.run(["go", "build", "-overlay=" + str(overlay), "-o", str(target / "wind.exe"), "./cmd/wind"],
    cwd=BUILD / "snapshot", env=dict(os.environ, GOWORK="off", GOTOOLCHAIN="local"), check=True)
shutil.copyfile(BUILD / "binaries/relay-native.exe", target / "relay-native.exe")
identity = {name: hashlib.sha256((target / name).read_bytes()).hexdigest()
    for name in ["wind.exe", "relay-native.exe"]}
identity["overlaySHA256"] = hashlib.sha256((HERE / "native-profile.go.txt").read_bytes()).hexdigest()
(BUILD / "binaries-profile.json").write_text(json.dumps(identity, indent=2) + "\n")
print(json.dumps(identity))
