"""Retain every repair attempt with source identity, output hashes and terminal traces."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(Path(__file__).parent.parent))
from evidence_export import write_public


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def reference(path):
    return {"path": path.relative_to(ROOT).as_posix(), "sha256": digest(path)}


def read(path):
    return json.loads(path.read_text(encoding="utf-8-sig"))


def trace(path):
    result = []
    if path.exists():
        for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
            try:
                result.append(json.loads(line))
            except ValueError:
                pass
    return result


def selected_events(events):
    return [event for event in events if (
        event.get("event") == "native_connectivity"
        and event.get("payload", {}).get("kind") == "content_recovery_requested"
    ) or (
        event.get("event") == "transfer_lifecycle"
        and event.get("payload", {}).get("stage") in {
            "file_settled", "revision_cleanup_settled", "job_settled"}
    ) or event.get("event") == "transfer_settled"]


def changed_sources():
    paths = set()
    for args in [
        ["diff", "--name-only", "--", "core", "connectivity", "engine"],
        ["ls-files", "--others", "--exclude-standard", "--", "core", "connectivity", "engine"],
    ]:
        paths.update(subprocess.check_output(["git", *args], cwd=ROOT, text=True).splitlines())
    return sorted(path for path in paths if path.endswith(".go"))


def export_build(name, sources):
    build = ROOT / ".tmp" / name
    identity_path = build / "identity.json"
    identity = read(identity_path)
    base = ROOT / ".tmp" / identity["base"]
    manifest = read(base / "sources.json")
    ledger_path = build / "recovery-ledger.json"
    ledger = read(ledger_path)
    result = {
        "name": name,
        "identitySource": reference(identity_path),
        "identity": identity,
        "baseRevision": manifest["revision"],
        "baseSourceManifest": reference(base / "sources.json"),
        "changedSourceHashes": {path: manifest["files"].get(path) for path in sources},
        "ledgerSource": reference(ledger_path),
        "casesSHA256": ledger["casesSHA256"],
        "harnessSHA256": ledger["harnessSHA256"],
        "runs": [],
    }
    for case in ledger["runs"]:
        run = {
            "case": {key: case[key] for key in (
                "endpoint", "direction", "sizeMiB", "connectivity", "windowsInterface",
                "fault", "deadlineSeconds") if key in case},
            "status": case["status"],
            "reports": [],
        }
        for report_name in case.get("reports", []):
            report_path = ROOT / report_name
            report = read(report_path)
            item = {
                "reportSource": reference(report_path),
                "remoteCleanup": report.get("remoteCleanup"),
                "expectedSHA256": report["sha256"],
                "rounds": [],
            }
            for index, transfer in enumerate(report.get("rounds", [])):
                trace_path = report_path.parent / f"receiver-{index}.ndjson"
                output = report_path.parent / f"output-{index}" / "probe.bin"
                events = trace(trace_path)
                item["rounds"].append({
                    "completionMs": transfer.get("completionMs"),
                    "exitCode": transfer.get("exitCode"),
                    "deadlineExceeded": transfer.get("deadlineExceeded"),
                    "outputBytes": output.stat().st_size if output.exists() else None,
                    "outputSHA256": digest(output) if output.exists() else None,
                    "outputMatches": output.exists() and digest(output) == report["sha256"],
                    "traceSource": reference(trace_path) if trace_path.exists() else None,
                    "traceIncomplete": "trace is incomplete" in transfer.get("stderr", "").lower(),
                    "terminalEvents": selected_events(events),
                })
            run["reports"].append(item)
        result["runs"].append(run)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("builds", nargs="+")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    sources = changed_sources()
    builds = [export_build(name, sources) for name in args.builds]
    write_public(args.output, {"builds": builds})
    for build in builds:
        runs = build["runs"]
        print(json.dumps({
            "build": build["name"], "runs": len(runs),
            "passed": sum(run["status"] == "passed" for run in runs),
            "allRemoteCleanupComplete": all(
                report["remoteCleanup"] == "complete" for run in runs for report in run["reports"]),
        }))


if __name__ == "__main__":
    main()
