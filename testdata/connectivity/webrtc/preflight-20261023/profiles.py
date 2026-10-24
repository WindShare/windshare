"""Keep diagnostic CPU samples separate from the unprofiled throughput baseline."""
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[4]
BUILD = ROOT / ".tmp/webrtc-preflight-20261023"
results = []
for report_path in sorted((BUILD / "runs").glob("*/report.json")):
    report = json.loads(report_path.read_text())
    if not report["preflight"].get("profile") or report["status"] != "passed":
        continue
    directory = report_path.parent
    profile = json.loads((directory / "0-cpu-profile.json").read_text())["profile"]
    nodes = {node["id"]: node["callFrame"] for node in profile["nodes"]}
    parents = {child: node['id'] for node in profile['nodes'] for child in node.get('children', [])}
    sampled = Counter()
    equality_callers = Counter()
    for node, delta in zip(profile["samples"], profile["timeDeltas"], strict=True):
        frame = nodes[node]
        sampled[(frame["functionName"], frame["url"].split("?")[0])] += delta
        if frame['functionName'] == 'equalBytes':
            equality_callers[nodes[parents[node]]['functionName']] += delta
    memory = [json.loads(line) for line in (directory / "sender-memory.ndjson").read_text().splitlines()]
    metrics = report["rounds"][0]["transferMetrics"]
    before = {process["id"]: process for process in metrics["before"]["processes"]["processInfo"]}
    cpu = Counter()
    for process in metrics["after"]["processes"]["processInfo"]:
        if process["id"] in before:
            cpu[process["type"]] += process["cpuTime"] - before[process["id"]]["cpuTime"]
    commands = {}
    for order in ("flat", "cum"):
        command = ["go", "tool", "pprof", "-top", "-nodecount=20"]
        if order == "cum":
            command += ["-cum"]
        command += [str(BUILD / "binaries-profile/wind.exe"), str(directory / "sender-cpu.pprof")]
        output = subprocess.check_output(command, cwd=ROOT, env=dict(os.environ, GOWORK="off", GOTOOLCHAIN="local"), text=True)
        (directory / ("sender-" + order + ".txt")).write_text(output)
        commands[order] = output
    total = sum(sampled.values())
    results.append({
        "report": str(report_path.relative_to(ROOT)), "connections": report["preflight"]["connections"],
        "diagnosticMBps": report["rounds"][0]["MBps"],
        "rendererSampleSeconds": total / 1e6,
        "rendererSelfSamples": [{"function": name, "url": url, "seconds": elapsed / 1e6,
            "percent": elapsed * 100 / total} for (name, url), elapsed in sampled.most_common(15)],
        "equalBytesCallerSeconds": {name: us / 1e6 for name, us in equality_callers.most_common()},
        "browserProcessCPUSeconds": dict(cpu),
        "sender": {
            "peakHeapAllocBytes": max(row["heapAlloc"] for row in memory),
            "peakHeapInuseBytes": max(row["heapInuse"] for row in memory),
            "allocatedBytes": memory[-1]["totalAlloc"] - memory[0]["totalAlloc"],
            "gcCycles": memory[-1]["gc"] - memory[0]["gc"],
            "gcPauseMs": (memory[-1]["pauseTotalNs"] - memory[0]["pauseTotalNs"]) / 1e6,
            "peakGoroutines": max(row["goroutines"] for row in memory),
            "pprof": commands,
        },
        "rawSHA256": {name: hashlib.sha256((directory / name).read_bytes()).hexdigest()
            for name in ["0-cpu-profile.json", "sender-cpu.pprof", "sender-memory.ndjson"]},
    })
(BUILD / "profiles.json").write_text(json.dumps({
    "scope": "Two trace-enabled diagnostic runs with CPU sampling and Go ReadMemStats every 25 ms. CDP renderer samples exclude browser networking and WebCrypto worker CPU; process counters cover surviving processes only. Sampled CPU is not blocked syscall wall time.",
    "runs": results,
}, indent=2) + "\n")
for result in results:
    print(json.dumps({key: value for key, value in result.items() if key not in ["rawSHA256", "sender"]}))
    print(result["sender"]["pprof"]["flat"])
