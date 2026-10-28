"""Expand the frozen fixture deterministically; retain a streaming identity."""
import hashlib
import json
import os
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory

ROOT = Path(__file__).resolve().parents[4]
BUILD = experiment_directory(ROOT, os.environ.get("WS_ADAPTIVE_BUILD", "webrtc-bottleneck-20261024-baseline"))
source = BUILD / "fixture.bin"
target = BUILD / "fixture-1gib.bin"
size = 1024 * 1024 * 1024
assert not target.exists()
digest = hashlib.sha256()
with target.open("xb") as output:
    remaining = size
    while remaining:
        with source.open("rb") as input:
            while remaining and (chunk := input.read(min(1024 * 1024, remaining))):
                output.write(chunk)
                digest.update(chunk)
                remaining -= len(chunk)
identity = {"bytes": size, "sha256": digest.hexdigest(), "source": str(source.relative_to(ROOT)), "method": "repeat frozen fixture prefix"}
target.with_suffix(".json").write_text(json.dumps(identity, indent=2) + "\n")
print(json.dumps(identity))
