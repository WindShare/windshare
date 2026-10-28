"""Read diagnostic CPU samples and SCTP queues; these runs are excluded from speed comparisons."""
import collections
import json
import os
from pathlib import Path
import statistics
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory

ROOT = Path(__file__).resolve().parents[4]
BUILD = experiment_directory(ROOT, os.environ.get("WS_ADAPTIVE_BUILD", "webrtc-bottleneck-20261024-baseline"))

def percentile(values, fraction):
    return sorted(values)[int((len(values) - 1) * fraction)] if values else None

summaries = []
for report_path in sorted((BUILD / "runs").glob("*/report.json")):
    report = json.loads(report_path.read_text())
    if not report.get("study", {}).get("profile") or not report["rounds"]:
        continue
    row = report["rounds"][0]
    directory = report_path.parent
    result = {"label": report.get("label"), "report": str(report_path.relative_to(ROOT)), "MBps": row["MBps"]}
    profile = json.loads((directory / "0-cpu-profile.json").read_text())["profile"]
    nodes = {node["id"]: node for node in profile["nodes"]}
    counts = collections.Counter()
    for node, dt in zip(profile["samples"], profile["timeDeltas"]):
        frame = nodes[node]["callFrame"]
        counts[(frame["functionName"], frame["url"].split("/src/")[-1])] += dt
    duration = sum(counts.values())
    result["rendererTop"] = [{"function": key[0], "file": key[1], "seconds": us / 1e6, "percent": us * 100 / duration}
        for key, us in counts.most_common(15)]
    before = {p["id"]: p["cpuTime"] for p in row["transferMetrics"]["before"]["processes"]["processInfo"]}
    process_cpu = collections.Counter()
    for p in row["transferMetrics"]["after"]["processes"]["processInfo"]:
        process_cpu[p["type"]] += p["cpuTime"] - before.get(p["id"], p["cpuTime"])
    result["browserProcessCPUSeconds"] = dict(process_cpu)
    start = row["timeOrigin"] + row["started"] + 1000
    end = row["timeOrigin"] + row["started"] + row["elapsedMs"] - 500
    result["native"] = []
    all_samples = []
    for path in sorted(directory.glob("native-stats-*.ndjson")):
        samples = [json.loads(line) for line in path.read_text().splitlines()]
        samples = [s for s in samples if start <= s["time"] <= end and s.get("queues")]
        if not samples:
            continue
        all_samples.extend(samples)
        result["native"].append({
            "peer": path.stem, "samples": len(samples),
            "bufferedMedian": statistics.median(s["buffered"] for s in samples),
            "pendingMedian": statistics.median(s["queues"]["pendingBytes"] for s in samples),
            "pendingZeroFraction": sum(s["queues"]["pendingBytes"] == 0 for s in samples) / len(samples),
            "inflightMedian": statistics.median(s["queues"]["inflightBytes"] for s in samples),
            "cwndMedian": statistics.median(s["sctp"]["congestionWindow"] for s in samples),
            "rwndMedian": statistics.median(s["sctp"]["receiverWindow"] for s in samples),
            "rttMedianMs": statistics.median(s["sctp"]["smoothedRoundTripTime"] * 1000 for s in samples),
            "lastCounters": samples[-1]["queues"],
        })
    if all_samples:
        ordered = sorted(all_samples, key=lambda s: s["time"])
        result["senderCPUSeconds"] = {kind: ordered[-1]["cpu"][kind] - ordered[0]["cpu"][kind] for kind in ["kernel", "user"]}
        result["senderCPUWindowSeconds"] = (ordered[-1]["time"] - ordered[0]["time"]) / 1000
    memory = [json.loads(line) for line in (directory / "sender-memory.ndjson").read_text().splitlines()]
    result["memory"] = {"allocatedBytes": memory[-1]["totalAlloc"] - memory[0]["totalAlloc"],
        "heapPeak": max(m["heapAlloc"] for m in memory), "gcCycles": memory[-1]["gc"] - memory[0]["gc"],
        "gcPauseMs": (memory[-1]["pauseTotalNs"] - memory[0]["pauseTotalNs"]) / 1e6}
    result["pprof"] = {}
    for profile_type in ["cpu", "block", "mutex", "allocs"]:
        path = directory / ("sender-" + profile_type + ".pprof")
        if not path.exists():
            continue
        for order in ["flat", "cum"]:
            command = ["go", "tool", "pprof", "-top", "-nodecount=18"]
            if order == "cum":
                command.append("-cum")
            command.extend([str(Path(report["binaryDirectory"]) / "wind.exe"), str(path)])
            result["pprof"][profile_type + "-" + order] = subprocess.check_output(command, text=True, stderr=subprocess.STDOUT)
    summaries.append(result)
(BUILD / "profile-analysis.json").write_text(json.dumps(summaries, indent=2) + "\n")
print(json.dumps({"profiles": len(summaries), "output": str(BUILD / "profile-analysis.json")}))
