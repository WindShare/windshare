"""Summarize all successes and failures without discarding slow observations."""
from collections import Counter, defaultdict
import hashlib
import json
import math
from pathlib import Path
import random
import re
from statistics import median

ROOT = Path(__file__).resolve().parents[4]
BUILD = ROOT / ".tmp/webrtc-preflight-20261023"
HERE = Path(__file__).parent

def span(row, left, right):
    return row[right] - row[left] if row.get(left) is not None and row.get(right) is not None else None

def metric_diff(row, name):
    metrics = row.get("transferMetrics", {})
    try:
        return next(x["value"] for x in metrics["after"]["metrics"]["metrics"] if x["name"] == name) - next(x["value"] for x in metrics["before"]["metrics"]["metrics"] if x["name"] == name)
    except (KeyError, StopIteration):
        return None

def socket_drops(report, row):
    result = []
    samples = report.get("socketSamples", [])
    suffix = ':' + str(row['repetition']) if report['preflight'].get('receivers', 1) > 1 else ''
    before = next((s for s in samples if s["phase"] == "before" + suffix), None)
    after = next((s for s in samples if s["phase"] == "after" + suffix), None)
    def find(sample, port):
        if sample is None:
            return None
        lines = sample["ss"].splitlines()
        for index, line in enumerate(lines):
            if re.search(r":" + str(port) + r"\b", line):
                memory = " ".join(lines[index:index + 2])
                found = re.search(r"[,\(]d(\d+)[,\)]", memory)
                if found:
                    return int(found.group(1))
        return None
    for peer in row["peers"]:
        port = peer.get("selected", {}).get("local", {}).get("port")
        first, last = find(before, port), find(after, port)
        result.append({"port": port, "before": first, "after": last,
            "delta": last - first if first is not None and last is not None else None})
    return result

def summarize():
    runs = []
    groups = defaultdict(list)
    for path in sorted((BUILD / "runs").glob("*/report.json")):
        report = json.loads(path.read_text())
        config = report["preflight"]
        label = report.get("label") or ""
        entry = {"label": label, "path": str(path.relative_to(ROOT)),
            "sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "status": report.get("status"),
            "error": report.get("error"), "config": config, "browser": report.get("browserVersion"),
            "cleanup": report.get("remoteCleanup"), "sink": report["sinkMode"],
            "trace": report["traceEnabled"], "sizeMiB": report["sizeMiB"],
            "slowWriteMs": report["slowWriteMs"], "interaction": report["interaction"],
            "topology": report["topology"], "tcpControls": report.get("tcpControls"),
            "outputControl": report.get("outputControl"), "rounds": []}
        entry['failedReceivers'] = []
        for failed_path in sorted(path.parent.glob('*-failure.json')):
            failed = json.loads(failed_path.read_text())
            entry['failedReceivers'].append({'file': failed_path.name, 'phase': failed.get('phase'),
                'admissions': failed.get('admissions'), 'progress': failed.get('progress'),
                'recoveryReasons': dict(Counter(event.get('reason') for event in failed.get('traces', [])
                    if event.get('eventName') == 'peer_recovery' and event.get('reason')))})
        sender_trace = path.parent / '0-sender.ndjson'
        entry['senderSocketCapacity'] = []
        if sender_trace.exists() and report.get('status') == 'failed':
            for line in sender_trace.read_text().splitlines():
                payload = json.loads(line).get('payload', {})
                capacity = payload.get('admission', {}).get('socket_capacity')
                if capacity is not None:
                    entry['senderSocketCapacity'].append({'kind': payload.get('kind'), 'at': payload.get('observed_at'), **capacity})
        failure_path = path.parent / "0-failure.json"
        if failure_path.exists():
            failure = json.loads(failure_path.read_text())
            entry["faultBoundary"] = {key: failure.get(key) for key in ["faultStateBefore", "faultStateAfter", "progress"]}
            entry["recoveryOutcomes"] = [
                {key: event.get(key) for key in ["eventName", "stage", "reason"]}
                for event in failure.get("traces", [])
                if event.get("eventName") == "peer_recovery"]
        for row in report.get("rounds", []):
            lane_counts = Counter(str(d.get("laneId", d.get("lane", "?"))) for d in row["dispatchTimeline"])
            timings = row.get("blockTimings", [])
            block_wait = [x for b in timings if (x := span(b, "started", "firstReceived")) is not None]
            auth_wait = [x for b in timings if (x := span(b, "lastReceived", "authenticated")) is not None]
            interactive = row.get("interactive", {})
            received = sum(b.get("receivedBytes", 0) for b in timings)
            compact = {key: row.get(key) for key in ["MBps","elapsedMs","firstWriteMs","setupMs",
                "activationToFirstWriteMs","activationToCompleteMs","size","sha256","outputBytes",
                "outputWriteMs","buffers","errors","researchEvents","workerStatus","lifecycle","timeOrigin","started"]}
            compact.update({"cpuScriptSeconds": metric_diff(row, "ScriptDuration"),
                "cpuTaskSeconds": metric_diff(row, "TaskDuration"), "laneDispatches": dict(lane_counts),
                "socketDrops": socket_drops(report, row),
                "attempts": len(timings), "failedAttempts": sum("failed" in b for b in timings),
                "receivedPayloadBytes": received,
                "responseWaitMedianMs": median(block_wait) if block_wait else None,
                "postReceiptMedianMs": median(auth_wait) if auth_wait else None,
                "previewMs": span(interactive, "startedMs", "completedMs"),
                "cancelMs": span(interactive, "abortRequestedMs", "completedMs"),
                "selectedPairs": [{"local": p.get("selected", {}).get("local"), "remote": p.get("selected", {}).get("remote")} for p in row["peers"]]})
            entry["rounds"].append(compact)
            group = (label.split("-")[0], config["connections"], report["sizeMiB"], report["slowWriteMs"], report["interaction"], report["sinkMode"], report["topology"], config.get("profile", False), config["late"], config["fault"], config.get("workflow", "file"), config.get("receivers", 1))
            if report.get("status") == "passed":
                groups[group].append(compact)
        if len(entry["rounds"]) > 1:
            begin = min(r["timeOrigin"] + r["started"] for r in entry["rounds"])
            end = max(r["timeOrigin"] + r["started"] + r["elapsedMs"] for r in entry["rounds"])
            entry["aggregateMBps"] = sum(r["outputBytes"] for r in entry["rounds"]) / (end - begin) / 1000
        runs.append(entry)
    summaries = []
    for group, rows in groups.items():
        summaries.append({"group": list(group), "count": len(rows),
            "MBps": [round(x["MBps"], 3) for x in rows],
            "medianMBps": median(x["MBps"] for x in rows),
            "firstWriteMedianMs": median(x["firstWriteMs"] for x in rows),
            "activationFirstMedianMs": median(x["activationToFirstWriteMs"] for x in rows),
            "activationCompleteMedianMs": median(x["activationToCompleteMs"] for x in rows),
            "peakReservedBytes": max(x["buffers"]["peakReserved"] for x in rows),
            "previewMs": [x["previewMs"] for x in rows if x["previewMs"] is not None],
            "cancelMs": [x["cancelMs"] for x in rows if x["cancelMs"] is not None]})
    baseline = sorted((r for r in runs if r["label"].startswith("baseline-") and r["status"] == "passed"),
        key=lambda r: int(r["label"].split("-")[-2]))
    paired = []
    if len(baseline) == 12:
        triples = [{r["config"]["connections"]: r["rounds"][0] for r in baseline[i:i+3]} for i in range(0, 12, 3)]
        for first, second in [(1, 2), (1, 4), (2, 4)]:
            logs = [math.log(triple[second]["MBps"] / triple[first]["MBps"]) for triple in triples]
            rng = random.Random(1023)
            estimates = sorted(math.exp(sum(rng.choice(logs) for _ in logs) / len(logs)) - 1 for _ in range(10000))
            paired.append({"connections": [first, second], "pairs": len(logs),
                "ratios": [math.exp(x) for x in logs], "wins": sum(x > 0 for x in logs),
                "geometricChangePercent": (math.exp(sum(logs) / len(logs)) - 1) * 100,
                "bootstrap95Percent": [estimates[249] * 100, estimates[9749] * 100]})
    result = {"revision": json.loads((BUILD / "sources.json").read_text())["revision"],
        "summaryGroupFields": ["stage", "connections", "sizeMiB", "slowWriteMs", "interaction", "sink", "topology", "profile", "late", "fault", "workflow", "receivers"],
        "successfulRuns": sum(r["status"] == "passed" for r in runs),
        "failedRuns": sum(r["status"] == "failed" for r in runs),
        "completedTransfers": sum(len(r["rounds"]) for r in runs),
        "summaries": summaries, "pairedBaseline": paired, "runs": runs}
    (BUILD / "results.json").write_text(json.dumps(result, indent=2) + "\n")
    for summary in summaries:
        print(json.dumps(summary))
    print(json.dumps({k: result[k] for k in ["successfulRuns","failedRuns","completedTransfers"]}))

if __name__ == "__main__":
    summarize()
