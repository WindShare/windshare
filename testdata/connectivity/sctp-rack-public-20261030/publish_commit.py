"""Publish only the sources and captures bound by a completed commit verification."""
import argparse
import json
from pathlib import Path
import statistics
import zipfile

from public_evidence import digest, write_archive


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--run", type=Path, required=True)
    args = parser.parse_args()
    run = args.run.resolve()
    root = Path(__file__).resolve().parent
    harness = run / "reproducer"
    verification = json.loads((run / "verification.json").read_text())
    if digest((root / "evidence.zip").read_bytes()) != verification["previous_archive_sha256"]:
        raise ValueError("Historical evidence changed after verification")
    patch = (harness / "candidate.patch").read_bytes()
    if digest(patch) != verification["candidate_patch_sha256"]:
        raise ValueError("Candidate patch changed after verification")
    for name, expected in verification["harness_sources_sha256"].items():
        if digest((harness / name).read_bytes()) != expected:
            raise ValueError(f"Tested harness source changed: {name}")
    tree = lambda folder: {p.relative_to(folder).as_posix(): digest(p.read_bytes())
                           for p in folder.rglob("*") if p.is_file()}
    if tree(run / "current") != tree(run / "replay"):
        raise ValueError("Tested candidate differs from the exact patch replay")
    captures = list((harness / "results").rglob("results.json"))
    if len(captures) != 1:
        raise ValueError("Expected one fresh paired matrix")
    results = json.loads(captures[0].read_text())
    for record in results:
        trace = captures[0].parent / (record["id"] + ".jsonl")
        if digest(trace.read_bytes()) != record["trace_sha256"]:
            raise ValueError(f"Capture changed: {trace.name}")
        if record["exit_code"] or not record["summary"]["complete"]:
            raise ValueError(f"Unsuccessful transfer: {record['id']}")
    binaries = json.loads((captures[0].parent / "binaries.json").read_text())
    for binary in binaries.values():
        if digest(Path(binary["path"]).read_bytes()) != binary["sha256"]:
            raise ValueError("Tested binary changed")
    aggregate = []
    for case in sorted({r["case"] for r in results}):
        for variant in ("upstream", "repaired"):
            rows = [r for r in results if r["case"] == case and r["variant"] == variant]
            times = [r["summary"]["last_drop_to_recovery_ms"] for r in rows
                     if "last_drop_to_recovery_ms" in r["summary"]]
            aggregate.append({
                "case": case, "variant": variant, "runs": len(rows),
                "t3_timeouts": sum(r["summary"]["sender_t3_timeouts"] for r in rows),
                "target_rack_marks": sum(r["summary"]["target_rack_marks"] for r in rows),
                "runs_with_later_send_ack_before_recovery": sum(
                    r.get("ack_witnesses_before_recovery", 0) > 0 for r in rows),
                "recovery_ms": {"min": min(times), "median": statistics.median(times),
                                "max": max(times)} if times else None,
            })
    commit = verification["candidate_commit"]
    short = commit[:7]
    double = [row for row in aggregate if row["case"] == "double-loss"]
    upstream, repaired = (row["recovery_ms"] for row in double)
    summary = {
        "baseline_commit": verification["baseline_commit"],
        "candidate_commit": commit,
        "candidate_tree": verification["candidate_tree"],
        "candidate_patch_sha256": digest(patch),
        "environment": results[0]["summary"]["os"],
        "go": verification["go"],
        "successful_transfers": len(results),
        "focused_passed_subtests": len(verification["current_focused_passed_subtests"]),
        "metric": "Milliseconds from the last deliberately lost target transmission to its next transmission.",
        "scope": "Recovery evidence for this exact commit; no general throughput claim.",
        "aggregate": aggregate,
    }
    readme = f"""# SCTP RACK verification: {short}

Baseline: `{verification['baseline_commit']}`
Candidate: `{commit}`

`candidate.patch` replays to the candidate's exact exported Git tree. The reproducer
is unchanged from the previous evidence archive. All {len(results)} {summary['environment']} loopback
transfers passed payload verification using {results[0]['summary']['go']} and default protocol timers.

## Results

Five double-loss runs per variant measured lost-retry to next-transmission delay:

| Version | Delay | T3 expirations |
| --- | --- | --- |
| Upstream | {upstream['min']:.1f}–{upstream['max']:.1f} ms | {double[0]['t3_timeouts']} |
| {short} | {repaired['min']:.1f}–{repaired['max']:.1f} ms | {double[1]['t3_timeouts']} |

The matrix also covers repeated loss, single/no loss, short flights, tail loss,
reordering, late originals, DATA/I-DATA, 50 ms RTT and fragmented messages.
These are recovery measurements, not throughput results. Windows wire timestamps
do not independently establish send ordering in every run; per-run ACK witnesses
and Pion RACK/T3 logs are retained. ACK ambiguity is checked by unit tests.

## Reproduce

Requires Git, Go, Python and PowerShell. Run from the extracted archive:

```powershell
git clone https://github.com/doraemonkeys/sctp ../sctp
git -C ../sctp checkout --detach {commit}
./build.ps1 -ForkPath ../sctp
python run_matrix.py --output results/new-run
go -C ../sctp test -race -count=1 ./...
```

Use a fresh output directory. The receiver verifies every payload; the wrapper
drops the original transmission and first retry of one TSN, preserving SACKs.

## Version audit

The previous `evidence.zip` tested a different patch. This commit permits retry
delivery evidence without an RTT estimate and judges retry ACK timing after all
RTT samples in the SACK have been processed. With this commit's regression tests,
the previous patch fails four subcases; this commit passes all
{summary['focused_passed_subtests']} subcases and the full race suite.

See `verification.json` for commands, revisions and hashes, `summary.json` for
aggregates, `results/` for traces, and `validation/` for test output.
The previous-candidate test log intentionally contains those four failures.
"""
    members = {name: (harness / name).read_bytes()
               for name in verification["harness_sources_sha256"]}
    members["candidate.patch"] = patch
    members["README.md"] = readme.encode()
    members["verification.json"] = (run / "verification.json").read_bytes()
    members["summary.json"] = (json.dumps(summary, indent=2) + "\n").encode()
    for directory in (harness / "results", run / "validation"):
        for path in directory.rglob("*"):
            if path.is_file():
                members[directory.name + "/" + path.relative_to(directory).as_posix()] = path.read_bytes()
    destination = root / f"commit-{short}-evidence.zip"
    published = write_archive(destination, members)
    # Verify the delivered hash graph after path and line-ending normalization.
    published_runs = json.loads(published[captures[0].relative_to(harness).as_posix()])
    for record in published_runs:
        trace_name = captures[0].parent.relative_to(harness).as_posix() + "/" + record["id"] + ".jsonl"
        if record["trace_sha256"] != digest(published[trace_name]):
            raise ValueError("Published trace digest mismatch")
    if digest(published["candidate.patch"]) != verification["candidate_patch_sha256"]:
        raise ValueError("Publication changed the commit patch")
    published_verification = json.loads(published["verification.json"])
    for name, expected in published_verification["harness_sources_sha256"].items():
        if digest(published[name]) != expected:
            raise ValueError(f"Published source digest mismatch: {name}")
    with zipfile.ZipFile(destination) as archive:
        if archive.testzip() is not None:
            raise ValueError("Invalid ZIP")
    (root / f"candidate-{short}.patch").write_bytes(published["candidate.patch"])
    (root / f"commit-{short}-summary.json").write_bytes(published["summary.json"])
    print(json.dumps({"archive": str(destination), "sha256": digest(destination.read_bytes()),
                      "successful_transfers": len(results), "double_loss": double}, indent=2))


if __name__ == "__main__":
    main()
