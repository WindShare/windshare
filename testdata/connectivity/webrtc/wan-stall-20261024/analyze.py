"""Compact, hashed evidence; private packet, path and process logs stay in .tmp."""
import argparse
import hashlib
import ipaddress
import json
from pathlib import Path
import statistics
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(Path(__file__).parent.parent))
from evidence_export import write_public
from experiment_paths import experiment_directory

BENCHMARK_NETWORK = ipaddress.ip_network("198.18.0.0/15")


def reference(path):
    return {"path": str(path.relative_to(ROOT)), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}


def events(path):
    if not path.exists():
        return []
    result = []
    for line in path.read_text(errors="replace").splitlines():
        try:
            result.append(json.loads(line))
        except ValueError:
            pass
    return result


def trace_summary(path):
    trace = events(path)
    progress = [{"ms": int(e["elapsed_ms"]), "bytes": int(e["payload"]["progress"]["verified_bytes"])}
                for e in trace if e.get("event") == "transfer_progress"]
    changes, previous = [], 0
    for point in progress:
        if point["bytes"] > previous:
            changes.append(point)
            previous = point["bytes"]
    operations = [{"ms": int(e["elapsed_ms"]), "correlation": e.get("correlation"), **e["payload"]}
                  for e in trace if e.get("event") == "protocol_operation"]
    blocks = [op for op in operations if op.get("request_kind") == "request_blocks"]
    pairs = []
    for event in trace:
        if event.get("event") != "native_connectivity" or event["payload"].get("kind") != "selected_pair":
            continue
        pair = event["payload"]["selected_pair"].copy()
        try:
            if ipaddress.ip_address(pair["local_address"]) in BENCHMARK_NETWORK:
                pair["localAddressClass"] = "benchmark-range; Windows inventory identifies Mihomo"
        except ValueError:
            pass
        pairs.append({"ms": int(event["elapsed_ms"]), **pair})
    end = progress[-1]["ms"] if progress else 0
    gaps = [b-a for a, b in zip([0] + [p["ms"] for p in changes], [p["ms"] for p in changes] + [end])]
    return {
        "source": reference(path) if path.exists() else None,
        "firstVerifiedMs": changes[0]["ms"] if changes else None,
        "lastVerifiedChangeMs": changes[-1]["ms"] if changes else None,
        "maxVerifiedBytes": max((p["bytes"] for p in progress), default=0),
        "lastProgressMs": end, "longestNoVerifiedProgressMs": max(gaps, default=end),
        "completedBlockRequests": sum(op.get("stage") == "receiver_completed" for op in blocks),
        "failedBlockRequests": [op for op in blocks if op.get("cause") not in (None, "none", "canceled", "superseded")],
        "leaseReleaseOperations": [op for op in operations if op.get("request_kind") == "release_lease"],
        "taskSettlements": [e for e in trace if e.get("event") == "engine_task_observed" and e.get("payload", {}).get("stage") == "task_settled"],
        "transferSettlements": [e for e in trace if e.get("event") == "transfer_settled"],
        "selectedPairs": pairs, "progress": progress,
    }


def diagnostic_summary(directory):
    result = []
    for path in sorted(directory.glob("research-sctp-*.ndjson")):
        rows = [r for r in events(path) if r.get("queues")]
        if not rows:
            continue
        start, end = rows[0], rows[-1]
        samples = [r for r in rows if r["state"] == "connected"]
        result.append({
            "source": reference(path), "samples": len(rows), "first": start, "last": end,
            "medianRTTSeconds": statistics.median(r["sctp"]["smoothedRoundTripTime"] for r in samples) if samples else None,
            "medianCwnd": statistics.median(r["sctp"]["congestionWindow"] for r in samples) if samples else None,
            "medianInflightBytes": statistics.median(r["queues"]["inflightBytes"] for r in samples) if samples else None,
            "minCwnd": min((r["sctp"]["congestionWindow"] for r in samples), default=0),
            "maxCwnd": max((r["sctp"]["congestionWindow"] for r in samples), default=0),
            "maxPendingBytes": max(r["queues"]["pendingBytes"] for r in rows),
            "maxInflightBytes": max(r["queues"]["inflightBytes"] for r in rows),
        })
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("builds", nargs="+")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    evidence = {"builds": [], "runs": []}
    for name in args.builds:
        build = experiment_directory(ROOT, name)
        identity = {"name": name}
        for filename in ["sources.json", "binaries.json", "identity.json"]:
            if (build / filename).exists():
                identity[filename] = reference(build / filename)
        identity["binaryHashes"] = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in (build / "binaries").glob("*") if p.is_file()}
        evidence["builds"].append(identity)
        reports = sorted([*build.glob("native-*/report.json"), *build.glob("linux-cross-*/report.json")])
        for path in reports:
            report = json.loads(path.read_text())
            row = {"build": name, "source": reference(path), "report": report,
                   "diagnostics": diagnostic_summary(path.parent), "traces": []}
            for index, round_ in enumerate(report.get("rounds", [])):
                row["traces"].append(trace_summary(path.parent / f"receiver-{index}.ndjson"))
                # A nonzero CLI exit can follow successful publication; retain both facts.
                published = path.parent / f"output-{index}/probe.bin"
                if published.exists():
                    digest = hashlib.sha256(published.read_bytes()).hexdigest()
                    row.setdefault("localPublishedOutputs", []).append({
                        "round": index, "size": published.stat().st_size, "sha256": digest,
                        "matchesExpected": digest == report["sha256"], "source": reference(published),
                    })
            row["researchArtifacts"] = [reference(p) for p in sorted(path.parent.glob("research-*"))]
            evidence["runs"].append(row)
            durations = [round(r.get("completionMs", 0)/1000, 3) for r in report.get("rounds", [])]
            print(json.dumps({"run": path.parent.name, "status": report.get("status"),
                              "seconds": durations, "maxVerified": [t["maxVerifiedBytes"] for t in row["traces"]]}))
    write_public(args.output, evidence)


if __name__ == "__main__":
    main()
