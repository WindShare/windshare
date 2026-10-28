"""Summarize complete files and fixed byte-percentile intervals, never peak samples."""
import json
import math
import os
from pathlib import Path
import statistics
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory
BUILD = experiment_directory(ROOT, os.environ.get("WS_ADAPTIVE_BUILD", "webrtc-bottleneck-20261024-baseline"))

def quantile(values, fraction):
    return sorted(values)[min(len(values) - 1, math.floor((len(values) - 1) * fraction))] if values else None

def summarize(path):
    report = json.loads(path.read_text())
    result = {"label": report.get("label"), "status": report.get("status"), "report": str(path.relative_to(ROOT)),
        "study": report.get("study"), "tcp": report.get("tcpControls"), "outputControl": report.get("outputControl"), "rounds": []}
    for row in report["rounds"]:
        total = row["size"]
        writes = row["writeTimeline"]
        points = [(row["started"], 0)]
        output = 0
        for write in writes:
            output += write["bytes"]
            points.append((write["end"], output))

        def time_at(size):
            for index, (at, done) in enumerate(points[1:], 1):
                if done >= size:
                    before, previous = points[index - 1]
                    return before + (at - before) * (size - previous) / (done - previous)
            return points[-1][0]

        def bytes_at(at):
            for index, (current, done) in enumerate(points[1:], 1):
                if current >= at:
                    before, previous = points[index - 1]
                    return previous + (done - previous) * (at - before) / max(0.001, current - before)
            return total

        start, end = time_at(total * .2), time_at(total * .8)
        blocks = row.get("blockTimings", [])
        auth = [b["authenticated"] - b["lastReceived"] for b in blocks if b.get("authenticated") is not None and b.get("lastReceived") is not None]
        demand = row.get("demandTimeline", [])
        changes = []
        for d in demand:
            if not changes or changes[-1]["peers"] != d["peers"]:
                changes.append({"seconds": (d["at"] - row["started"]) / 1000, "peers": d["peers"]})
        intervals = []
        for offset in range(5, math.floor(row["elapsedMs"] / 1000) - 5, 5):
            begin = row["started"] + offset * 1000
            finish = begin + 5000
            intervals.append({"fromSeconds": offset, "MBps": (bytes_at(finish) - bytes_at(begin)) / 5e6})
        r = {
            "fullMBps": row["MBps"], "clickMBps": total / row.get("activationToCompleteMs", row["elapsedMs"]) / 1000,
            "middle20to80MBps": .6 * total / (end - start) / 1000, "middleSeconds": (end - start) / 1000,
            "elapsedSeconds": row["elapsedMs"] / 1000, "bytes": total, "peers": len(row["peers"]),
            "peakConcurrentPaths": max((d["peers"] for d in demand), default=len(row["peers"])),
            "parallelismDecisions": [t for t in row.get("protocolTraces", []) if t.get("stage") == "parallelism"],
            "firstOutputMs": row.get("activationToFirstWriteMs"), "sha256": row["sha256"],
            "outputWriteMs": row["outputWriteMs"], "peerChanges": changes,
            "authenticationAfterReceiveMs": {"p50": quantile(auth, .5), "p95": quantile(auth, .95), "max": max(auth, default=0)},
            "fiveSecondOutputIntervals": intervals,
        }
        result["rounds"].append(r)
    return result

if __name__ == "__main__":
    reports = [summarize(p) for p in sorted((BUILD / "runs").glob("*/report.json"))]
    (BUILD / "analysis.json").write_text(json.dumps(reports, indent=2) + "\n")
    for report in reports:
        if len(sys.argv) > 1 and sys.argv[1] not in (report["label"] or ""):
            continue
        for row in report["rounds"]:
            print(json.dumps({"label": report["label"], **{k: row[k] for k in [
                "fullMBps", "middle20to80MBps", "middleSeconds", "peers", "outputWriteMs", "peerChanges", "fiveSecondOutputIntervals"]}}))
