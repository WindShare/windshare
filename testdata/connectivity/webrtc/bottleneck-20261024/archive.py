"""Archive measured results and identities without copying large private runtime artifacts."""
import collections
import hashlib
import json
import os
from pathlib import Path
import platform
import sys
import statistics
import subprocess

ROOT = Path(__file__).resolve().parents[4]
HERE = Path(__file__).parent
sys.path.insert(0, str(HERE.parent))
from evidence_export import write_public
from experiment_paths import experiment_directory, experiment_label
BUILD = experiment_directory(ROOT, os.environ.get("WS_ADAPTIVE_BUILD", "webrtc-bottleneck-20261024-baseline"))

def read(path):
    return json.loads(path.read_text(encoding="utf-8"))

def identity(path):
    return {"path": path.relative_to(ROOT).as_posix(), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}

def spread(values):
    return {"n": len(values), "median": statistics.median(values), "min": min(values), "max": max(values)}

analysis = read(BUILD / "analysis.json")
summaries = {row["label"]: row for row in analysis}
ledgers = [read(p) for p in sorted(BUILD.glob("*-ledger.json"))]
configs = {r["label"]: r for ledger in ledgers for r in ledger["runs"]}
runs = []
for path in sorted((BUILD / "runs").glob("*/report.json")):
    report = read(path)
    label = report["label"]
    summary = summaries[label]
    config = configs.get(label, {})
    study = report.get("study", {})
    paths = study.get("paths", 1 if label.endswith("-single") else 0)
    policy = study.get("policy", "original")
    profile = study.get("profile", False)
    row = {
        "label": label, "raw": identity(path), "status": report["status"],
        "classification": "invalid-control" if report["status"] != "passed" else "diagnostic" if profile else "performance",
        "fixtureMiB": report["sizeMiB"], "topology": report["topology"], "sink": report["sinkMode"],
        "pathsPolicy": paths or "adaptive", "policy": policy, "profile": profile,
        "browser": report.get("browserVersion"), "tcpControls": report.get("tcpControls", []),
        "outputControl": report.get("outputControl"), "cleanup": report.get("remoteCleanup"),
        "binaryIdentity": study.get("binaries", read(BUILD / "binaries.json")),
        "error": report.get("error"), "rounds": summary["rounds"],
    }
    for result, original in zip(row["rounds"], report["rounds"]):
        result.update({
            "workerStatus": original["workerStatus"], "lifecycle": original["lifecycle"],
            "outputBytes": original["outputBytes"], "errors": original["errors"],
            "selectedPairs": [peer["selected"] for peer in original["peers"]],
        })
        assert original["workerStatus"] == "Succeeded" and original["lifecycle"] == "published"
        assert original["size"] == original["outputBytes"] and not original["errors"]
        expected = read(BUILD / "fixture-1gib.json")["sha256"] if original["size"] == 1024**3 else hashlib.sha256((BUILD / "fixture.bin").read_bytes()).hexdigest()
        assert original["sha256"] == expected
    if report["topology"] == "lan-isolated":
        assert report["remoteCleanup"]["status"] == "complete"
    runs.append(row)

def group(size, paths, policy="original", sink="opfs", topology="lan-isolated"):
    selected = [r for r in runs if r["classification"] == "performance"
        and r["fixtureMiB"] == size and r["pathsPolicy"] == paths and r["policy"] == policy
        and r["sink"] == sink and r["topology"] == topology]
    values = [r["rounds"][0] for r in selected]
    return {"labels": [r["label"] for r in selected],
        **{k: spread([v[k] for v in values]) for k in ["clickMBps", "middle20to80MBps", "firstOutputMs"]}}

groups = {
    "256MiB-single": group(256, 1),
    "256MiB-adaptive": group(256, "adaptive"),
    "1GiB-adaptive": group(1024, "adaptive"),
    "1GiB-fixed-three": group(1024, 3),
    "1GiB-fixed-two": group(1024, 2),
    "1GiB-success-no-cooldown": group(1024, "adaptive", "success-no-cooldown"),
    "1GiB-recovery-window": group(1024, "adaptive", "recovery-window"),
}
# Keep the four alternating pairs separate from the later exploratory single-path control.
paired_labels = [r["label"] for r in runs if r["label"].startswith("large-initial-")]
for arm in ["single", "adaptive"]:
    selected = [r for r in runs if r["label"] in paired_labels and r["label"].endswith("-" + arm)]
    groups["256MiB-paired-" + arm] = {
        "labels": [r["label"] for r in selected],
        **{k: spread([r["rounds"][0][k] for r in selected]) for k in ["clickMBps", "middle20to80MBps", "firstOutputMs"]},
    }

cases = {}
for suite in ["screen", "long-and-profile", "confirmation", "trial-window"]:
    cases[suite] = read(BUILD / (suite + ".json"))
    for case in cases[suite]:
        if "fixture" in case:
            case["fixture"] = Path(case["fixture"]).name
write_public(HERE / "cases.json", cases)

profiles = read(BUILD / "profile-analysis.json")
wall = read(BUILD / "wall-analysis.json")
sources = read(BUILD / "sources.json")
source_changes = []
for name, digest in sources["files"].items():
    path = ROOT / name
    actual = hashlib.sha256(path.read_bytes()).hexdigest() if path.exists() else None
    if actual != digest:
        source_changes.append({"path": name, "expected": digest, "actual": actual})
assert all(c["path"] == "docs/plans/webrtc-throughput.md" for c in source_changes), source_changes
write_public(HERE / "sources.json", sources)

slow = next(r for r in runs if r["label"] == "confirmation-6-large-original-4")
slow_report = read(ROOT / slow["raw"]["path"])
blocks_by_lane = collections.defaultdict(list)
for block in slow_report["rounds"][0]["blockTimings"]:
    blocks_by_lane[block["laneId"]].append(block)
dispatches_by_epoch = collections.defaultdict(list)
for dispatch in slow_report["rounds"][0]["dispatchTimeline"]:
    dispatches_by_epoch[dispatch["laneEpoch"]].append(dispatch)
sparse_epochs = {epoch: rows for epoch, rows in dispatches_by_epoch.items() if len(rows) <= 3}
sparse_blocks = []
for dispatches in sparse_epochs.values():
    for dispatch in dispatches:
        sparse_blocks.extend(b for b in blocks_by_lane[dispatch["laneId"]]
            if b["index"] == dispatch["localBlockIndex"] and abs(b["started"] - dispatch["at"]) < 10)
tcp = [c["MBps"] for r in runs if r["status"] == "passed" and r["topology"] == "lan-isolated" for c in r["tcpControls"]]
output_controls = [r["outputControl"]["MBps"] for r in runs if r["outputControl"] and r["status"] == "passed"]
results = {
    "date": "2026-10-24", "units": {"MBps": "decimal MB/s", "MiB": 1024**2},
    "scope": "Frozen working-tree build, native Windows sender to physical Linux Chrome over isolated direct LAN UDP; production read/seal/authentication/ordered output, existing test plan-settlement adapter. Not UI, resume, durability or WAN evidence.",
    "timing": {
        "clickMBps": "Content activation to output writer closure. Excludes connection/catalog setup before activation and post-close full-file hash.",
        "middle20to80MBps": "60% of file bytes divided by time between interpolated 20% and 80% cumulative output-write completions. Fixed definition across all arms.",
        "fiveSecondOutputIntervals": "Interior 5-second output intervals after the first 5 seconds; no best-window selection.",
        "profile": "CPU/queue/wall instrumentation runs are diagnostics and excluded from performance aggregates.",
    },
    "attempts": {"total": len(runs), "passed": sum(r["status"] == "passed" for r in runs),
        "invalidControl": sum(r["classification"] == "invalid-control" for r in runs),
        "diagnostic": sum(r["classification"] == "diagnostic" for r in runs),
        "sameHost": sum(r["topology"] != "lan-isolated" for r in runs)},
    "build": {"name": experiment_label(BUILD.name), "revision": sources["revision"], "sourceManifest": identity(BUILD / "sources.json"),
        "fileCount": len(sources["files"]), "binaries": read(BUILD / "binaries.json"),
        "sourceChangesSinceFreeze": source_changes,
        "gitIndexUnchanged": hashlib.sha256((ROOT / ".git/index").read_bytes()).hexdigest() == sources["indexSHA256"],
        "versions": {"python": platform.python_version(),
            "go": subprocess.check_output(["go", "version"], text=True).strip(),
            "node": subprocess.check_output(["node", "--version"], text=True).strip()}},
    "validation": {"hygiene": {"passed": "== hygiene: PASS" in (BUILD / "hygiene.log").read_text(encoding="utf-8"), "log": identity(BUILD / "hygiene.log")}},
    "controls": {"tcpMBps": spread(tcp), "bareOutputMBps": spread(output_controls)},
    "groups": groups, "runs": runs, "profiles": profiles, "wall": wall,
    "underSampledTrial": {"label": slow["label"], "laneDispatchCounts": {k: len(v) for k, v in blocks_by_lane.items()},
        "epochDispatchCounts": {k: len(v) for k, v in dispatches_by_epoch.items()},
        "sparseEpochs": sparse_epochs, "blockTimings": sparse_blocks},
    "artifacts": {
        "ledgers": [identity(p) for p in sorted(BUILD.glob("*-ledger.json"))],
        "drivers": [identity(p) for p in sorted((BUILD / "drivers").glob("*"))],
        "profiles": [identity(p) for p in sorted((BUILD / "runs").glob("*/*")) if p.suffix in {".pprof", ".ndjson"} or p.name.endswith("-cpu-profile.json")],
        "fixture1GiB": read(BUILD / "fixture-1gib.json"),
        "helpers": [identity(p) for p in sorted(HERE.iterdir()) if p.suffix in {".py", ".mjs"}],
    },
    "limitations": [
        "One Windows desktop and one Linux Wi-Fi host; physical-network variance remains.",
        "No stable default-policy 30 MB/s claim; no 40 MB/s full-product claim.",
        "Successful/no-cooldown and longer-settling overlays had inconsistent gains and were not adopted.",
        "Windows UDP call wall time does not isolate driver, filter, kernel or scheduling contributions.",
        "The fourth-path control exceeded the production recovery-slot reserve and is invalid; all other controls request at most three.",
        "Early drivers imported routes.mjs; later drivers freeze it and record its SHA256. Original policy and fixed-path behavior were unchanged when the later recovery-window branch was added.",
        "Raw runtime artifacts and frozen builds remain in ignored .tmp; their hashes/references are retained here.",
    ],
}
write_public(HERE / "results.json", results)
for name, value in groups.items():
    print(name, "n", value["clickMBps"]["n"], "full", round(value["clickMBps"]["median"], 2),
        "middle", round(value["middle20to80MBps"]["median"], 2))
print(json.dumps({"attempts": results["attempts"], "controls": results["controls"], "sourceChanges": source_changes}))
