"""Bind fresh recovery evidence to an exact fork commit; preserve earlier captures."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import zipfile

BASELINE = "6ec762ebc1106ae8c5ce1a69208a98dfcfdddaaf"
FOCUSED = "TestRACK(LostRetransmission|RetransmissionDeliveryEvidence|RetransmissionRejoinsLossScan|RetryEvidenceUsesWholeSACKRTT)$"
REGRESSION_FILE = "association_rack_retransmission_test.go"
HISTORICAL_FAILURES = {
    "TestRACKRetransmissionDeliveryEvidence/retry-without-rtt/cumulative",
    "TestRACKRetransmissionDeliveryEvidence/retry-without-rtt/selective",
    "TestRACKRetryEvidenceUsesWholeSACKRTT/cumulative",
    "TestRACKRetryEvidenceUsesWholeSACKRTT/selective",
}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def git(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args])


def save_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf8")


def snapshot(repo, commit, destination):
    destination.mkdir()
    with zipfile.ZipFile(io.BytesIO(git(repo, "archive", "--format=zip", commit))) as archive:
        for entry in archive.infolist():
            path = (destination / entry.filename).resolve()
            if not path.is_relative_to(destination):
                raise ValueError("Archive member escapes the snapshot")
        archive.extractall(destination)


def command(args, cwd, log, records, expected=0):
    print("Running:", " ".join(map(str, args)), flush=True)
    # Exported sources are intentionally independent of the enclosing WindShare checkout.
    env = os.environ | {"GIT_CEILING_DIRECTORIES": str(cwd.parent)}
    result = subprocess.run(args, cwd=cwd, env=env, capture_output=True, timeout=240)
    log.write_bytes(result.stdout + result.stderr)
    records.append({"command": list(map(str, args)), "cwd": str(cwd),
                    "exit_code": result.returncode, "log": str(log)})
    if result.returncode != expected:
        raise RuntimeError(f"Unexpected exit {result.returncode}; see {log}")
    return result.stdout


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--fork", type=Path, required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    fork, out = args.fork.resolve(), args.output.resolve()
    commit = git(fork, "rev-parse", args.commit + "^{commit}").decode().strip()
    out.mkdir(parents=True, exist_ok=False)
    logs = out / "validation"
    logs.mkdir()
    harness = out / "reproducer"
    harness.mkdir()
    source_zip = root / "evidence.zip"
    with zipfile.ZipFile(source_zip) as archive:
        old_patch = archive.read("candidate.patch")
        for name in archive.namelist():
            path = Path(name)
            if len(path.parts) == 1 and (path.suffix == ".go" or name in {
                    "go.mod", "go.sum", "run_matrix.py", "build.ps1"}):
                (harness / name).write_bytes(archive.read(name))
    patch = git(fork, "diff", "--binary", BASELINE, commit)
    (harness / "candidate.patch").write_bytes(patch)
    (out / "previous-candidate.patch").write_bytes(old_patch)
    current, replay, previous = (out / name for name in ("current", "replay", "previous"))
    snapshot(fork, commit, current)
    snapshot(fork, BASELINE, replay)
    snapshot(fork, BASELINE, previous)
    commands = []
    command(["git", "apply", str(harness / "candidate.patch")],
            replay, logs / "apply-current.txt", commands)
    command(["git", "apply", str(out / "previous-candidate.patch")],
            previous, logs / "apply-previous.txt", commands)
    # Compare the whole exported tree, not only the changed production file.
    tree = lambda directory: {p.relative_to(directory).as_posix(): digest(p.read_bytes())
                              for p in directory.rglob("*") if p.is_file()}
    if tree(current) != tree(replay):
        raise RuntimeError("Applying the published patch does not reproduce the commit tree")
    old_test_hash = digest((previous / REGRESSION_FILE).read_bytes())
    (previous / REGRESSION_FILE).write_bytes((current / REGRESSION_FILE).read_bytes())
    old_output = command(["go", "test", "-race", "-count=1", "-json", "-run", FOCUSED, "."],
                         previous, logs / "previous-with-current-tests.jsonl", commands, expected=1)
    old_events = [json.loads(line) for line in old_output.splitlines()]
    failed = {e["Test"] for e in old_events if e.get("Action") == "fail" and "/" in e.get("Test", "")}
    if failed != HISTORICAL_FAILURES:
        raise RuntimeError(f"Unexpected historical candidate failures: {failed}")
    output = command(["go", "test", "-race", "-count=1", "-json", "-run", FOCUSED, "."],
                     current, logs / "current-focused.jsonl", commands)
    events = [json.loads(line) for line in output.splitlines()]
    passed = [e["Test"] for e in events if e.get("Action") == "pass" and "/" in e.get("Test", "")]
    command(["go", "test", "-race", "-count=1", "./..."],
            current, logs / "current-full-race.txt", commands)
    (harness / "repair.mod").write_bytes((harness / "go.mod").read_bytes())
    (harness / "repair.sum").write_bytes((harness / "go.sum").read_bytes())
    command(["go", "mod", "edit", "-modfile=repair.mod",
             "-replace=github.com/pion/sctp=" + current.as_posix()],
            harness, logs / "module-replacement.txt", commands)
    suffix = ".exe" if sys.platform == "win32" else "-linux"
    for variant in ("upstream", "repaired"):
        flags = [] if variant == "upstream" else ["-modfile=repair.mod", "-ldflags=-X main.variant=repaired"]
        command(["go", "build", "-trimpath", *flags, "-o", ".build/" + variant + suffix, "."],
                harness, logs / (variant + "-build.txt"), commands)
    command(["go", "vet", "."], harness, logs / "reproducer-vet.txt", commands)
    matrix_path = "results/" + platform.system().lower()
    command([sys.executable, "run_matrix.py", "--output", matrix_path],
            harness, logs / "matrix.txt", commands)
    runs = json.loads((harness / matrix_path / "results.json").read_text())
    if not runs or any(r["exit_code"] or not r["summary"]["complete"] for r in runs):
        raise RuntimeError("Recovery matrix contains failed or incomplete transfers")
    manifest = {
        "baseline_commit": BASELINE,
        "candidate_commit": commit,
        "candidate_tree": git(fork, "rev-parse", commit + "^{tree}").decode().strip(),
        "candidate_patch_sha256": digest(patch),
        "patch_replays_exact_commit_tree": True,
        "previous_archive_sha256": digest(source_zip.read_bytes()),
        "previous_candidate_patch_sha256": digest(old_patch),
        "previous_test_file_before_overlay_sha256": old_test_hash,
        "previous_test_overlay_sha256": digest((current / REGRESSION_FILE).read_bytes()),
        "historical_candidate_failed_current_subtests": sorted(failed),
        "current_focused_passed_subtests": passed,
        "current_full_race_passed": True,
        "public_api_successful_transfers": len(runs),
        "go": subprocess.check_output(["go", "version"]).decode().strip(),
        "harness_sources_sha256": {p.name: digest(p.read_bytes()) for p in sorted(harness.iterdir())
                                  if p.suffix == ".go" or p.name in {"go.mod", "go.sum", "run_matrix.py", "build.ps1"}},
        "commands": commands,
    }
    save_json(out / "verification.json", manifest)
    print(json.dumps({k: v for k, v in manifest.items() if k not in {
        "commands", "harness_sources_sha256", "current_focused_passed_subtests"}}, indent=2))


if __name__ == "__main__":
    main()
