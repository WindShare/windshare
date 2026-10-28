"""Derived bottleneck facts from retained per-process samples and experiment ledgers."""
import argparse
import json
from pathlib import Path
import sys

HERE = Path(__file__).parent
ROOT = HERE.parents[3]
sys.path.insert(0, str(HERE))
from analyze import diagnostic_summary, events, reference
sys.path.insert(0, str(HERE.parent))
from evidence_export import write_public
from experiment_paths import experiment_directory


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("builds", nargs="+")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--proof-build", help="Build containing deterministic reproductions; defaults to the first build")
    args = parser.parse_args()
    result = {"runs": [], "ledgers": []}
    for name in args.builds:
        build = experiment_directory(ROOT, name)
        cases = {}
        socket_path = build / "linux-udp-sockets.ndjson"
        socket_samples = events(socket_path)
        for ledger_path in sorted(build.glob("*-ledger.json")):
            ledger = json.loads(ledger_path.read_text())
            result["ledgers"].append({"source": reference(ledger_path), "ledger": ledger})
            for row in ledger["runs"]:
                for report_path in row.get("reports", []):
                    cases[str(Path(report_path))] = row
        for report_path in sorted([*build.glob("native-*/report.json"), *build.glob("linux-cross-*/report.json")]):
            report = json.loads(report_path.read_text())
            directory = report_path.parent
            row = {"build": name, "source": reference(report_path), "endpoint": report.get("endpoint", "public"),
                   "case": cases.get(str(report_path.relative_to(ROOT))),
                   "status": report.get("status"), "sctp": [], "scheduler": []}
            row["udpCounterDeltas"] = []
            for round_ in report.get("rounds", []):
                before, after = round_.get("udpCountersBefore", {}), round_.get("udpCountersAfter", {})
                row["udpCounterDeltas"].append({host: {family: {key: value - before[host][family][key]
                    for key, value in counters.items() if isinstance(value, (int, float)) and key in before[host][family]}
                    for family, counters in families.items() if family in before.get(host, {})}
                    for host, families in after.items() if host in before})
            row["socketBuffers"] = [value for p in directory.glob("research-socket-buffer-*.ndjson") for value in events(p)]
            owned = [sock for sample in socket_samples for sock in sample["sockets"]
                     if sock["directory"] == report.get("remoteDirectory")]
            if owned:
                drops = {}
                for sock in owned:
                    key = (sock["pid"], sock["inode"])
                    drops[key] = max(drops.get(key, 0), sock["drops"])
                row["linuxUDPSockets"] = {"source": reference(socket_path), "sockets": len(drops),
                    "sampledDropLowerBound": sum(drops.values()),
                    "maxReceiveQueueBytes": max(sock["receiveQueueBytes"] for sock in owned)}
            for sample in diagnostic_summary(directory):
                first, last = sample["first"], sample["last"]
                seconds = (last["timeMs"] - first["timeMs"]) / 1000
                send_ns = last["queues"]["writeNs"] - first["queues"]["writeNs"]
                row["sctp"].append({
                    "source": sample["source"], "elapsedSeconds": seconds,
                    "last": last, "medianRTTSeconds": sample["medianRTTSeconds"],
                    "medianCwnd": sample["medianCwnd"], "medianInflightBytes": sample["medianInflightBytes"],
                    "minCwnd": sample["minCwnd"], "maxCwnd": sample["maxCwnd"],
                    "maxPendingBytes": sample["maxPendingBytes"], "maxInflightBytes": sample["maxInflightBytes"],
                    "sendCallWallFraction": send_ns / (seconds * 1e9) if seconds else None,
                })
            for path in sorted(directory.glob("research-scheduler-*.ndjson")):
                samples = events(path)
                if samples:
                    row["scheduler"].append({"source": reference(path), "last": samples[-1],
                        "maxUsefulBytes": max(s["demand"]["UsefulBytes"] for s in samples),
                        "maxOutputBytes": max(s["demand"]["OutputBytes"] for s in samples),
                        "timeline": [{"seconds": (s["timeMs"] - samples[0]["timeMs"]) / 1000,
                                      "usefulBytes": s["demand"]["UsefulBytes"],
                                      "outputBytes": s["demand"]["OutputBytes"],
                                      "inFlightBytes": s["demand"]["InFlightBytes"],
                                      "unfinishedBytes": s["demand"]["UnfinishedBytes"]} for s in samples],
                        "lastTen": samples[-10:]})
            result["runs"].append(row)
    proof_build = experiment_directory(ROOT, args.proof_build or args.builds[0])
    result["reproduction"] = {path.parent.name: {
        "source": reference(path), "fixture": reference(path.parent / "route_exhaustion_test.go"),
        "results": json.loads(path.read_text())}
        for path in sorted(proof_build.glob("route-exhaustion*/results.json"))}
    result["networkInventory"] = [reference(path) for name in args.builds
        for filename in ["windows-network.json", "remote-inventory.json", "receive-buffer-inventory.json"]
        if (path := experiment_directory(ROOT, name) / filename).exists()]
    write_public(args.output, result)
    print(json.dumps({"runs": len(result["runs"]), "output": str(args.output)}))


if __name__ == "__main__":
    main()
