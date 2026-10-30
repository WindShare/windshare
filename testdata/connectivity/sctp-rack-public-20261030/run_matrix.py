"""Run bounded, paired public-API experiments; retain traces and every failed run."""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import subprocess
import time

def cases():
    return [
        ("double-loss", ["-drops", "2"], 5),
        ("triple-loss", ["-drops", "3", "-messages", "800"], 2),
        ("single-loss", ["-drops", "1", "-messages", "80"], 2),
        ("no-loss", ["-drops", "0", "-messages", "80"], 2),
        ("short-flight", ["-drops", "2", "-messages", "40"], 2),
        ("tail", ["-scenario", "tail"], 2),
        ("reordering", ["-scenario", "reorder", "-messages", "80"], 2),
        ("late-original", ["-scenario", "ambiguous"], 3),
        ("data-chunks", ["-interleaving=false"], 2),
        ("rtt-50ms", ["-delay", "25ms"], 2),
        ("fragmented", ["-size", "4096", "-messages", "320"], 2),
    ]

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--bin-dir", default=".build")
    parser.add_argument("--suffix", default=".exe" if platform.system() == "Windows" else "-linux")
    parser.add_argument("--output", default="results/windows")
    parser.add_argument("--only", default="")
    args = parser.parse_args()
    out = Path(args.output)
    out.mkdir(parents=True, exist_ok=True)
    selected = set(args.only.split(",")) if args.only else set()
    records = []
    paths = {v: (Path(args.bin_dir) / (v + args.suffix)).resolve() for v in ("upstream", "repaired")}
    manifest = {v: {"path": str(p), "sha256": hashlib.sha256(p.read_bytes()).hexdigest()} for v, p in paths.items()}
    (out / "binaries.json").write_text(json.dumps(manifest, indent=2) + "\n")
    for case, flags, rounds in cases():
        if selected and case not in selected:
            continue
        for number in range(1, rounds + 1):
            variants = ("upstream", "repaired") if number % 2 else ("repaired", "upstream")
            for variant in variants:
                run_id = f"{case}-{number}-{variant}"
                trace = out / (run_id + ".jsonl")
                cmd = [str(paths[variant]), *flags, "-trace", str(trace)]
                begin = time.monotonic()
                result = subprocess.run(cmd, capture_output=True, text=True, timeout=12)
                record = {"id": run_id, "case": case, "variant": variant, "exit_code": result.returncode, "command": cmd,
                          "wall_seconds": round(time.monotonic() - begin, 3), "stderr": result.stderr}
                if result.stdout.strip():
                    record["summary"] = json.loads(result.stdout.strip().splitlines()[-1])
                events = [json.loads(line) for line in trace.read_text().splitlines()] if trace.exists() else []
                summary = record.get("summary", {})
                sends = summary.get("target_send_ms", [])
                drops = summary.get("drops_requested", 0)
                if summary.get("scenario") == "loss" and drops >= 2 and len(sends) > drops:
                    record["ack_witnesses_before_recovery"] = sum(
                        e["event"] == "later_send_acknowledged" and
                        e["send_ms"] > sends[drops-1] and e["ms"] < sends[drops]
                        for e in events
                    )
                record["collateral_drops"] = sum(e["event"] == "collateral_drop" for e in events)
                record["trace_sha256"] = hashlib.sha256(trace.read_bytes()).hexdigest() if trace.exists() else None
                records.append(record)
                with (out / "runs.jsonl").open("a", encoding="utf8") as ledger:
                    ledger.write(json.dumps(record) + "\n")
                print(f"{run_id}: exit={result.returncode}, T3={summary.get('sender_t3_timeouts')}, "
                      f"recovery={summary.get('last_drop_to_recovery_ms')}, witnesses={record.get('ack_witnesses_before_recovery')}", flush=True)
    (out / "results.json").write_text(json.dumps(records, indent=2) + "\n", encoding="utf8")
    if any(r["exit_code"] for r in records):
        raise SystemExit(1)

if __name__ == "__main__":
    main()
