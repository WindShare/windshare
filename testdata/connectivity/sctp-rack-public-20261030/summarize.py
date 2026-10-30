"""Freeze measurements and source hashes, retaining failed exploratory runs separately."""
import hashlib
import json
from pathlib import Path
import statistics

from public_evidence import write_archive

root = Path(__file__).resolve().parent
groups = ["final-windows", "final-linux", "lan-v3"]
rows = []
for group in groups:
    for run in json.loads((root / "results" / group / "results.json").read_text()):
        if run["exit_code"] or run.get("receiver_exit_code", 0):
            raise SystemExit(f"Failed final run: {group}/{run['id']}")
        if not run["summary"]["complete"]:
            raise SystemExit(f"Incomplete final run: {group}/{run['id']}")
        rows.append({"environment": group, **run})
aggregate = []
for group in groups:
    cases = sorted({r["case"] for r in rows if r["environment"] == group})
    for case in cases:
        for variant in ["upstream", "repaired"]:
            runs = [r for r in rows if r["environment"] == group and r["case"] == case and r["variant"] == variant]
            times = [r["summary"]["last_drop_to_recovery_ms"] for r in runs if "last_drop_to_recovery_ms" in r["summary"]]
            aggregate.append({
                "environment": group, "case": case, "variant": variant, "runs": len(runs),
                "t3_timeouts": sum(r["summary"]["sender_t3_timeouts"] for r in runs),
                "recovery_ms": {"min": min(times), "median": statistics.median(times), "max": max(times)} if times else None,
            })
sources = [p for p in root.iterdir() if not p.name.endswith("_test.py") and
           (p.suffix in [".go", ".py", ".ps1", ".patch"] or p.name in ["go.mod", "go.sum"])]
digest = lambda p: hashlib.sha256(p.read_bytes()).hexdigest()
summary = {
    "baseline": "6ec762ebc1106ae8c5ce1a69208a98dfcfdddaaf",
    "go": "1.27.3",
    "final_successful_runs": len(rows),
    "metric": "Milliseconds from the last deliberately dropped target transmission to the next target transmission.",
    "scope": "Controlled loss recovery; no general throughput claim.",
    "aggregate": aggregate,
    "sources_sha256": {p.name: digest(p) for p in sorted(sources)},
    "current_binaries_sha256": {p.name: digest(p) for p in sorted((root / ".build").iterdir()) if p.is_file() and (p.suffix == ".exe" or p.name.endswith("-linux"))},
}
(root / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf8")
members = {p.relative_to(root).as_posix(): p.read_bytes()
           for p in sorted((root / "results").rglob("*"))
           if p.is_file() and not p.relative_to(root / "results").parts[0].startswith("tail-followup-")}
members.update({p.name: p.read_bytes() for p in sources + [root / "summary.json", root / "README.md"]})
published = write_archive(root / "evidence.zip", members)
(root / "summary.json").write_bytes(published["summary.json"])
print(json.dumps({"final_successful_runs": len(rows), "double_loss": [a for a in aggregate if a["case"] == "double-loss"],
                  "archive_bytes": (root / "evidence.zip").stat().st_size}, indent=2))
