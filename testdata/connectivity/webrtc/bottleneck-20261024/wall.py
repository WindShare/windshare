"""Report wall time inside existing writes. Concurrent connection times must not be added as elapsed time."""
import json
import os
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory

ROOT = Path(__file__).resolve().parents[4]
BUILD = experiment_directory(ROOT, os.environ.get("WS_ADAPTIVE_BUILD", "webrtc-bottleneck-20261024-baseline"))
results = []
for path in sorted((BUILD / "runs").glob("*/report.json")):
    report = json.loads(path.read_text())
    if not report["rounds"]:
        continue
    row = report["rounds"][0]
    start = row["timeOrigin"] + row["started"] + 1000
    end = row["timeOrigin"] + row["started"] + row["elapsedMs"] - 500
    result = {"label": report.get("label"), "report": str(path.relative_to(ROOT)), "peers": [], "MBps": row["MBps"]}
    all_samples = []
    for file in sorted(path.parent.glob("native-stats-*.ndjson")):
        samples = [json.loads(line) for line in file.read_text().splitlines()]
        samples = [s for s in samples if start <= s["time"] <= end and "udp" in s and s.get("queues")]
        if len(samples) < 2:
            continue
        a, b = samples[0], samples[-1]
        all_samples.extend(samples)
        delta = {k: b["queues"][k] - a["queues"][k] for k in ["writeCount", "writeNs", "writeSlow"]}
        elapsed = (b["time"] - a["time"]) / 1000
        result["peers"].append({"id": b["id"], "windowSeconds": elapsed, "writes": delta["writeCount"],
            "sendCallSeconds": delta["writeNs"] / 1e9, "sendCallFraction": delta["writeNs"] / 1e9 / elapsed,
            "meanSendMicroseconds": delta["writeNs"] / max(1, delta["writeCount"]) / 1000,
            "writesAtLeast1ms": delta["writeSlow"]})
    if not all_samples:
        continue
    all_samples.sort(key=lambda s: s["time"])
    a, b = all_samples[0], all_samples[-1]
    delta = {k: b["udp"][k] - a["udp"][k] for k in b["udp"]}
    result["udpAggregate"] = {**delta, "meanMicroseconds": delta["nanoseconds"] / max(1, delta["writes"]) / 1000,
        "summedCallSeconds": delta["nanoseconds"] / 1e9, "windowSeconds": (b["time"] - a["time"]) / 1000}
    results.append(result)
(BUILD / "wall-analysis.json").write_text(json.dumps(results, indent=2) + "\n")
for result in results:
    print(result["label"], "MB/s", round(result["MBps"], 2), "UDP us/call", round(result["udpAggregate"]["meanMicroseconds"], 2),
        "send fractions", [round(p["sendCallFraction"], 3) for p in result["peers"]])
