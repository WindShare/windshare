"""Serial native experiments. Preserve every attempt, including setup failures."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import shutil
import sys
import time

ROOT = Path(__file__).resolve().parents[4]
HARNESS = ROOT / "testdata/connectivity/webrtc/adaptive-20261023/cross-host.py"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("cases", type=Path)
    parser.add_argument("--build", required=True)
    parser.add_argument("--name", required=True)
    parser.add_argument("--public-endpoint", required=True)
    args = parser.parse_args()
    build = ROOT / ".tmp" / args.build
    ledger_path = build / (args.name + "-ledger.json")
    if ledger_path.exists():
        raise FileExistsError(ledger_path)
    cases = json.loads(args.cases.read_text())
    ledger = {"build": args.build, "casesSHA256": hashlib.sha256(args.cases.read_bytes()).hexdigest(),
              "harnessSHA256": hashlib.sha256(HARNESS.read_bytes()).hexdigest(),
              "startedMs": time.time() * 1000, "runs": []}

    def save():
        ledger_path.write_text(json.dumps(ledger, indent=2) + "\n")

    shutil.copyfile(args.cases, build / (args.name + "-cases.json"))
    shutil.copyfile(HARNESS, build / (args.name + "-cross-host.py"))
    shutil.copyfile(Path(__file__), build / (args.name + "-matrix.py"))
    shutil.copyfile(Path(__file__).parent / "linux-cross.py", build / (args.name + "-linux-cross.py"))
    save()
    for index, case in enumerate(cases):
        assert hashlib.sha256(HARNESS.read_bytes()).hexdigest() == ledger["harnessSHA256"], "Harness changed mid-batch"
        endpoint = args.public_endpoint if case["endpoint"] == "public" else case["endpoint"]
        before = set(build.glob("*/report.json"))
        command = [sys.executable, str(HARNESS), endpoint, "--build", args.build,
                   "--direction", case["direction"], "--size-mib", str(case["sizeMiB"]),
                   "--repeats", "1", "--connectivity", case.get("connectivity", "p2p-only"),
                   "--deadline-seconds", str(case.get("deadlineSeconds", 180)),
                   "--block-size", str(case.get("blockSize", 1048576)), "--fault", case.get("fault", "none"),
                   "--receive-buffer", str(case.get("receiveBufferBytes", 0)), "--remote-interface", case.get("lanInterface", "")]
        if case.get("topology") == "linux-cross":
            command = [sys.executable, str(Path(__file__).parent / "linux-cross.py"), "--build", args.build,
                       "--public-endpoint", args.public_endpoint, "--sender", case["sender"],
                       "--size-mib", str(case["sizeMiB"]), "--repeats", "1",
                       "--deadline-seconds", str(case.get("deadlineSeconds", 180)),
                       "--lan-interface", case.get("lanInterface", "")]
            if case.get("sameHost"):
                command.append("--same-host")
        row = dict(case, startedMs=time.time() * 1000, status="running")
        ledger["runs"].append(row)
        save()
        print(json.dumps({"event": "starting", "index": index + 1, "case": case}), flush=True)
        with (build / f"{args.name}-{index + 1}.log").open("w") as log:
            environment = dict(os.environ)
            if "windowsInterface" in case:
                environment["WS_WAN_INTERFACE"] = case["windowsInterface"]
            result = subprocess.run(command, cwd=ROOT, env=environment, stdout=log, stderr=subprocess.STDOUT)
        row.update(exitCode=result.returncode, finishedMs=time.time() * 1000)
        reports = sorted(set(build.glob("*/report.json")) - before)
        row["reports"] = [str(path.relative_to(ROOT)) for path in reports]
        row["status"] = "passed" if result.returncode == 0 else "failed"
        for report_path in reports:
            report = json.loads(report_path.read_text())
            if report.get("status") != "passed":
                row["status"] = "failed"
            row["transfer"] = [{key: value for key, value in round_.items()
                                if key in ("completionMs", "exitCode", "deadlineExceeded", "MBpsIncludingStartup", "sha256")}
                               for round_ in report["rounds"]]
        save()
        print(json.dumps({"event": "finished", "index": index + 1, **row}), flush=True)
    ledger["finishedMs"] = time.time() * 1000
    save()


if __name__ == "__main__":
    main()
