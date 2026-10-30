"""Audit both tail matrices and package them without changing the 102-run archive."""
import json
from pathlib import Path

from public_evidence import hash_updates, write_archive

from run_tail import aggregate, digest, write_json

ROOT = Path(__file__).resolve().parent
GROUPS = ("tail-followup-windows", "tail-followup-linux")
VARIANTS = ("upstream", "repaired")


def audit(group):
    folder = ROOT / "results" / group
    records = json.loads((folder / "results.json").read_text())
    manifest = json.loads((folder / "manifest.json").read_text())
    expected_runs = manifest["rounds"] * len(manifest["delays"]) * len(manifest["drops"]) * len(VARIANTS)
    if len(records) != expected_runs or len({record["id"] for record in records}) != expected_runs:
        raise SystemExit(f"Incomplete matrix: {group}")
    for record in records:
        trace = folder / (record["id"] + ".jsonl")
        if digest(trace) != record["trace_sha256"] or not record["valid"]:
            raise SystemExit(f"Invalid evidence: {group}/{record['id']}")
        events = [json.loads(line) for line in trace.read_text().splitlines()]
        sends = [event for event in events if event["event"] == "data_send" and event["target"]]
        drops = record["drops"]
        summary = record["summary"]
        expected = (int(drops > 0), max(drops - 1, 0), 0, 0)
        observed = (record["sender_tlp_probes"], summary["sender_t3_timeouts"],
                    summary["target_rack_marks"], summary["later_send_witnesses"])
        if observed != expected or len(sends) != drops + 1:
            raise SystemExit(f"Unexpected recovery path (retained for review): {group}/{record['id']}: {observed}")
        milestones = record["recovery_milestones"]
        if len(milestones) != drops:
            raise SystemExit(f"Missing timer evidence: {group}/{record['id']}")
        for attempt, (milestone, send) in enumerate(zip(milestones, sends[1:]), start=1):
            # Event sequence proves causality even when Windows timestamps tie.
            if not sends[attempt - 1]["seq"] < milestone["event_seq"] < send["seq"]:
                raise SystemExit(f"Ambiguous timer ordering: {group}/{record['id']}")
            marker = "PTO fired:" if attempt == 1 else "T3-rtx timed out:"
            if marker not in milestone["message"]:
                raise SystemExit(f"Wrong recovery timer: {group}/{record['id']}")
    return records


def main():
    groups = {group: audit(group) for group in GROUPS}
    sources = [ROOT / name for name in ("run_tail.py", "summarize_tail.py", "public_evidence.py", "candidate.patch", "go.mod", "go.sum")]
    sources += sorted(ROOT.glob("*.go"))
    summary = {
        "baseline": json.loads((ROOT / "summary.json").read_text())["baseline"],
        "runs": sum(len(records) for records in groups.values()),
        "metric": "Milliseconds from original target send to first non-dropped target send; excludes delivery and ACK latency.",
        "scope": "One unfragmented final message after warmup; unchanged SCTP defaults; nominal RTTs 20 and 50 ms.",
        "aggregate": [{"environment": group, **row} for group, records in groups.items() for row in aggregate(records)],
        "sources_sha256": {path.name: digest(path) for path in sources},
        "original_evidence_sha256": digest(ROOT / "evidence.zip"),
    }
    write_json(ROOT / "tail-summary.json", summary)
    members = {path.relative_to(ROOT).as_posix(): path.read_bytes()
               for group in GROUPS for path in sorted((ROOT / "results" / group).iterdir()) if path.is_file()}
    members.update({path.name: path.read_bytes()
                    for path in sources + [ROOT / "tail-summary.json", ROOT / "summary.json", ROOT / "README.md"]})
    published = write_archive(ROOT / "tail-evidence.zip", members, hash_updates(ROOT / "evidence.zip"))
    (ROOT / "tail-summary.json").write_bytes(published["tail-summary.json"])
    print(json.dumps({"verified_runs": summary["runs"], "archive_sha256": digest(ROOT / "tail-evidence.zip")}))
    for group, records in groups.items():
        for drops in range(1, 4):
            times = [record["summary"]["original_to_recovery_ms"] for record in records if record["drops"] == drops]
            print(f"{group}: drops={drops}, original_to_recovery_ms={min(times):.3f}..{max(times):.3f}")


if __name__ == "__main__":
    main()
