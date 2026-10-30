"""Isolate tail recovery using the exact binaries from the original evidence."""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import subprocess
import time

VARIANTS = ("upstream", "repaired")
DELAYS = ("10ms", "25ms")
DROP_COUNTS = range(4)
RUN_TIMEOUT_SECONDS = 12


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf8")


def measure(binary, trace, delay, drops):
    command = [str(binary), "-scenario", "tail", "-drops", str(drops),
               "-delay", delay, "-trace", str(trace)]
    begin = time.monotonic()
    try:
        result = subprocess.run(command, capture_output=True, text=True,
                                timeout=RUN_TIMEOUT_SECONDS)
        record = {"exit_code": result.returncode, "stdout": result.stdout,
                  "stderr": result.stderr}
    except subprocess.TimeoutExpired as exc:
        decode = lambda value: value.decode(errors="replace") if isinstance(value, bytes) else value
        record = {"exit_code": None, "stdout": decode(exc.stdout) or "",
                  "stderr": decode(exc.stderr) or "", "error": "process timeout"}
    record.update(command=command, wall_seconds=round(time.monotonic() - begin, 3))
    events = [json.loads(line) for line in trace.read_text().splitlines()] if trace.exists() else []
    summaries = [event for event in events if event["event"] == "summary"]
    summary = summaries[-1] if summaries else {}
    sends = [event for event in events if event["event"] == "data_send" and event["target"]]
    milestones = [
        event for event in events if event["event"] == "pion" and
        ("[sender] PTO fired:" in event["message"] or "[sender] T3-rtx timed out:" in event["message"])
    ]
    origin = sends[0]["ms"] if sends else 0
    # Trace events distinguish a probe from T3; elapsed time alone cannot do that.
    record.update(
        summary=summary,
        trace_sha256=digest(trace) if trace.exists() else None,
        target_send_offsets_ms=[round(event["ms"] - origin, 3) for event in sends],
        recovery_milestones=[{"offset_ms": round(event["ms"] - origin, 3),
                              "event_seq": event["seq"], "message": event["message"]}
                             for event in milestones],
        sender_tlp_probes=sum("PTO fired:" in event["message"] for event in milestones),
        valid=(record["exit_code"] == 0 and summary.get("complete") is True
               and summary.get("verified_bytes", 0) > 0
               and len(sends) >= drops + 1
               and sum(event["action"] == "drop" for event in sends) == drops),
    )
    return record


def aggregate(records):
    rows = []
    for delay in DELAYS:
        for drops in DROP_COUNTS:
            for variant in VARIANTS:
                runs = [r for r in records if (r["delay"], r["drops"], r["variant"]) == (delay, drops, variant)]
                recovered = [r["summary"]["original_to_recovery_ms"] for r in runs
                             if "original_to_recovery_ms" in r["summary"]]
                rows.append({
                    "delay": delay, "drops": drops, "variant": variant,
                    "runs": len(runs), "valid": sum(r["valid"] for r in runs),
                    "sender_tlp_probes": sum(r["sender_tlp_probes"] for r in runs),
                    "sender_t3_timeouts": sum(r["summary"].get("sender_t3_timeouts", 0) for r in runs),
                    "target_rack_marks": sum(r["summary"].get("target_rack_marks", 0) for r in runs),
                    "later_send_witnesses": sum(r["summary"].get("later_send_witnesses", 0) for r in runs),
                    "original_to_recovery_ms": [min(recovered), max(recovered)] if recovered else None,
                })
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, help="new directory; existing evidence is never overwritten")
    parser.add_argument("--bin-dir", default=".build")
    parser.add_argument("--suffix", default=".exe" if platform.system() == "Windows" else "-linux")
    parser.add_argument("--rounds", type=int, default=3)
    args = parser.parse_args()
    if args.rounds < 1:
        parser.error("--rounds must be positive")
    root = Path(__file__).resolve().parent
    original = json.loads((root / "summary.json").read_text())
    binaries = {v: (Path(args.bin_dir) / (v + args.suffix)).resolve() for v in VARIANTS}
    manifest = {}
    for variant, binary in binaries.items():
        actual = digest(binary)
        expected = original["current_binaries_sha256"][binary.name]
        if actual != expected:
            raise SystemExit(f"Binary differs from original evidence: {binary}")
        manifest[variant] = {"path": str(binary), "sha256": actual}
    out = Path(args.output).resolve()
    out.mkdir(parents=True, exist_ok=False)
    write_json(out / "manifest.json", {
        "baseline": original["baseline"], "platform": platform.platform(),
        "binaries": manifest, "runner_sha256": digest(Path(__file__)),
        "original_summary_sha256": digest(root / "summary.json"),
        "rounds": args.rounds, "delays": DELAYS, "drops": list(DROP_COUNTS),
        "metric": "Milliseconds from original target send to first non-dropped target send.",
    })
    records = []
    for delay in DELAYS:
        for drops in DROP_COUNTS:
            for number in range(1, args.rounds + 1):
                # Alternate order so one variant does not always get the first run.
                variants = VARIANTS if number % 2 else VARIANTS[::-1]
                for variant in variants:
                    run_id = f"tail-{delay}-drops{drops}-{number}-{variant}"
                    record = {"id": run_id, "delay": delay, "drops": drops,
                              "variant": variant, **measure(binaries[variant], out / f"{run_id}.jsonl", delay, drops)}
                    records.append(record)
                    with (out / "runs.jsonl").open("a", encoding="utf8") as ledger:
                        ledger.write(json.dumps(record) + "\n")
                    summary = record["summary"]
                    print(f"{run_id}: valid={record['valid']}, TLP={record['sender_tlp_probes']}, "
                          f"T3={summary.get('sender_t3_timeouts')}, "
                          f"original_to_recovery_ms={summary.get('original_to_recovery_ms')}", flush=True)
    write_json(out / "results.json", records)
    write_json(out / "summary.json", {"runs": len(records), "valid": sum(r["valid"] for r in records),
                                     "aggregate": aggregate(records)})
    if not all(record["valid"] for record in records):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
