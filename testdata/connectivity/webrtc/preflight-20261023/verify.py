"""Verify preserved evidence and export compact, reviewable artifacts."""
import ast
from collections import Counter
import hashlib
import json
from pathlib import Path
import re
import sys
import subprocess

ROOT = Path(__file__).resolve().parents[4]
HERE = Path(__file__).parent
sys.path.insert(0, str(HERE.parent))
from evidence_export import write_public
BUILD = ROOT / ".tmp/webrtc-preflight-20261023"

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

sources = json.loads((BUILD / "sources.json").read_text())
changed = []
for relative, expected in sources["files"].items():
    assert digest(BUILD / "snapshot" / relative) == expected, "Snapshot changed: " + relative
    if digest(ROOT / relative) != expected:
        changed.append(relative)
assert set(changed) <= {"docs/plans/webrtc-throughput.md"}, changed
index_hash = digest(ROOT / ".git/index")
# The shared desktop may stage reviewed documentation while experiments run.
# Record index changes without resetting anyone else's staging decisions.
staged_paths = subprocess.check_output(["git", "diff", "--cached", "--name-only"], cwd=ROOT, text=True).splitlines()
assert subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip() == sources["revision"]
fixture = (BUILD / "fixture.bin").read_bytes()
assert hashlib.sha256(fixture).hexdigest() == sources["fixture"]["sha256"]

counts = Counter()
remote = []
fixture_hashes = {}
for path in sorted((BUILD / "runs").glob("*/report.json")):
    report = json.loads(path.read_text())
    counts[report["status"]] += 1
    if report.get("remote"):
        assert report["remoteCleanup"]["status"] == "complete", path
        remote.append({key: report["remote"][key] for key in ["pid", "profile"]})
    for row in report["rounds"]:
        size = round(report["sizeMiB"] * 1024 * 1024)
        expected = fixture_hashes.setdefault(str(size), hashlib.sha256(fixture[:size]).hexdigest())
        assert row["size"] == row["outputBytes"] == size, path
        assert row["sha256"] == expected, path
        assert row["workerStatus"] == "Succeeded" and row["lifecycle"] == "published", path
        assert row["buffers"]["reserved"] == 0 and not row["errors"], path
        counts["transfers"] += 1

for path in HERE.glob("*.py"):
    ast.parse(path.read_text(), filename=str(path))
for path in HERE.glob("*.mjs"):
    subprocess.run(["node", "--check", str(path)], check=True)
for name in ["results.json", "profiles.json"]:
    write_public(HERE / name, json.loads((BUILD / name).read_text()))

strip_ansi = lambda value: re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", value)
web = strip_ansi((BUILD / "validation-web.log").read_text())
go = (BUILD / "validation-go.log").read_text()
assert "99 passed" in web
assert len(re.findall(r"^ok\s+github.com/windshare/windshare/", go, re.MULTILINE)) == 4

identity = {
    "revision": sources["revision"],
    "trackedSnapshotFiles": len(sources["files"]),
    "snapshotManifestSHA256": digest(BUILD / "sources.json"),
    "initialGitIndexSHA256": sources['indexSHA256'],
    "gitIndexSHA256AtVerification": index_hash,
    "stagedPathsAtVerification": staged_paths,
    "sourceChangesAfterMeasurement": changed,
    "initialGitStatus": sources["status"],
    "binaries": json.loads((BUILD / "binaries.json").read_text()),
    "profileBinaries": json.loads((BUILD / "binaries-profile.json").read_text()),
    "fixturePrefixSHA256": fixture_hashes,
    "runCounts": dict(counts), "remoteCleanupsAcknowledged": len(remote),
    "harnessSHA256": {path.name: digest(path) for path in sorted(HERE.iterdir())
        if path.suffix in [".py", ".mjs", ".ps1", ".txt"]},
    "evidenceSHA256": {name: digest(HERE / name) for name in ["results.json", "profiles.json"]},
    "validation": {
        "web": {"files": 10, "tests": 99, "status": "passed", "logSHA256": digest(BUILD / "validation-web.log"),
            "command": "pnpm --dir web exec vitest run test/connectivity/peer-budget.test.ts test/connectivity/peer-slot-lifecycle.test.ts test/connectivity/v2-receiver-policy.test.ts test/connectivity/v2-peer-recovery.test.ts test/connectivity/peer-tab-admission.test.ts test/content/admission-wake.test.ts test/content/range-window.test.ts test/content/block-buffers.test.ts test/content/receive-window.test.ts test/content/v2-lane-set.test.ts"},
        "go": {"command": "GOWORK=off GOTOOLCHAIN=local go test -short ./core/transfer/lanes ./connectivity/v2peer/peerset ./core/liveshare ./engine",
            "packages": 4, "status": "passed", "logSHA256": digest(BUILD / "validation-go.log")},
        "syntax": "Python AST and node --check passed",
    },
}
for name in ['cleanup-audit.json', 'local-process-audit.json']:
    if (BUILD / name).exists():
        identity[name] = json.loads((BUILD / name).read_text())
if (BUILD / 'validation-hygiene.exit').exists():
    exit_code = int((BUILD / 'validation-hygiene.exit').read_text().strip())
    identity['validation']['hygiene'] = {'command': 'make hygiene', 'exitCode': exit_code,
        'status': 'passed' if exit_code == 0 else 'failed', 'logSHA256': digest(BUILD / 'validation-hygiene.log')}
write_public(HERE / "identity.json", identity)
(BUILD / "owned-remote-profiles.json").write_text(json.dumps(remote))
print(json.dumps({"verified": dict(counts), "snapshotFiles": len(sources["files"]), "changed": changed}))
