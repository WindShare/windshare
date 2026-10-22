"""Extract bounded, reviewable evidence from the complete workflow reports."""
from pathlib import Path
import hashlib
import json
import statistics

ROOT = Path(__file__).resolve().parents[4]
DESTINATION = Path(__file__).resolve().parent / "results.json"
runs = []
for path in sorted((ROOT / ".tmp/webrtc-slowdown-20261022").glob("*/report.json")):
    report = json.loads(path.read_text())
    if not str(report.get("label", "")).startswith("allocation-") or report.get("status") != "passed":
        continue
    rounds = []
    for row in report["rounds"]:
        first_payload = row["firstWriteMs"]
        if report.get("allocationWorkflow") == "zip":
            # ZIP headers precede content; compare the first full payload block,
            # not an early metadata write, with the ordinary-file measurements.
            first_payload = next(write["at"] for write in row["writeTimeline"]
                if write["bytes"] >= min(row["size"], report["blockMiB"] * 1024 * 1024)) - row["started"]
        interaction = row.get("interactive", {})
        delay = interaction.get("completedMs", 0) - interaction.get("startedMs", 0)
        cancel = interaction.get("completedMs", 0) - interaction.get("abortRequestedMs", 0)
        peers = []
        for peer in row["peers"]:
            selected = peer["selected"]
            peers.append({
                "local": {key: selected["local"].get(key) for key in ("address", "port", "protocol", "candidateType")},
                "remote": {key: selected["remote"].get(key) for key in ("address", "port", "protocol", "candidateType")},
                "rttSeconds": selected["pair"].get("currentRoundTripTime"),
                "bytesReceived": selected["pair"].get("bytesReceived"),
            })
        rounds.append({
            "MBps": row["MBps"], "elapsedMs": row["elapsedMs"],
            "firstPayloadWriteMs": first_payload, "outputWriteMs": row["outputWriteMs"],
            "cpuSeconds": row["cpuSeconds"], "bytes": row["size"], "sha256": row["sha256"],
            "archiveBytes": row.get("archiveBytes"), "worker": row["workerStatus"], "lifecycle": row["lifecycle"],
            "interaction": interaction.get("result"),
            "interactionMs": delay if interaction else None,
            "cancellationMs": cancel if "abortRequestedMs" in interaction else None,
            "dispatches": row["dispatches"], "peers": peers, "browserErrors": row["errors"],
        })
    runs.append({
        "report": path.relative_to(ROOT).as_posix(), "label": report["label"],
        "workflow": report.get("allocationWorkflow", "file"), "sizeMiB": report["sizeMiB"],
        "blockMiB": report["blockMiB"], "slowWriteMs": report["slowWriteMs"],
        "interaction": report["interaction"], "browser": report["browserVersion"],
        "topology": report["topology"], "scope": report["scope"],
        "medianMBps": statistics.median(row["MBps"] for row in rounds),
        "rounds": rounds,
    })
binaries = {}
for path in (ROOT / ".tmp/single-connection-implementation/baseline").glob("*.exe"):
    binaries[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()
groups = {}
for label in ("allocation-before", "allocation-after", "allocation-reserve-file", "allocation-reserve-reserve-zero"):
    values = [row["MBps"] for run in runs if run["label"] == label and run["sizeMiB"] == 256
              and (run["blockMiB"] == 1 if label in ("allocation-before", "allocation-after") else True)
              for row in run["rounds"]]
    if values:
        groups[label] = {"MBps": values, "medianMBps": statistics.median(values)}
result = {
    "date": "2026-10-22", "units": "decimal MB/s; binary MiB fixtures",
    "sender": "Windows production CLI (lab)", "receiver": "Linux Chromium (lab)",
    "control": "One UDP host-candidate connection; isolated external browser DNS; unchanged send watermarks and Pion dependencies.",
    "senderBinarySHA256": binaries,
    "profileScope": "CDP CPU deltas cover page imports/setup, transfer and post-close verification; transfer elapsed excludes setup and hash. Write durations are measured inside transfer. These coarse counters do not isolate an SCTP, DTLS or native-syscall bottleneck.",
    "comparison": groups, "runs": runs,
    "recovery": {
        "crossProcess": "durable-recovery.periodic.spec.ts: two tests passed, including forced Chromium process termination",
        "directZip": "Production browser tests verify partial member resume, revision change rollback, checkpoint promotion faults and published ZIP payloads",
        "scope": "Recovery uses real OPFS/IndexedDB and browser processes; independent of the LAN throughput probes. No WAN or native acceleration claim.",
    },
}
DESTINATION.write_text(json.dumps(result, indent=2) + "\n")
print(json.dumps({"runs": len(runs), "comparison": groups}, indent=2))
