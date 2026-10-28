"""Test the scheduler hypothesis without a real clock or network."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory

ROOT = Path(__file__).resolve().parents[4]
HERE = Path(__file__).parent


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--build", required=True)
    parser.add_argument("--name", default="route-exhaustion")
    args = parser.parse_args()
    build = experiment_directory(ROOT, args.build)
    snapshot = build / "snapshot"
    output = build / args.name
    output.mkdir()
    test = output / "route_exhaustion_test.go"
    test.write_bytes((HERE / "route-exhaustion_test.go.txt").read_bytes())
    source = snapshot / "core/transfer/lanes/allocation.go"
    original = source.read_text()
    anchor = "if s.contentSuspensions[state.identity.ID] != nil || !s.contentRoutePolicy.Allows(state.route) {"
    assert original.count(anchor) == 1
    control = output / "allocation.go"
    control.write_text(original.replace(anchor,
        "if !s.contentRoutePolicy.Allows(state.route) { continue }\n\t\tif s.contentSuspensions[state.identity.ID] != nil {"))
    rows = []
    for name, replacement in [("baseline", None), ("permanent-exclusion-control", control)]:
        replacements = {str(snapshot / "core/transfer/lanes/z_research_exhaustion_test.go"): str(test)}
        if replacement:
            replacements[str(source)] = str(replacement)
        overlay = output / (name + ".json")
        overlay.write_text(json.dumps({"Replace": replacements}, indent=2))
        log = output / (name + ".log")
        with log.open("w") as stream:
            result = subprocess.run(["go", "test", "-overlay=" + str(overlay), "-v", "-count=5",
                                     "-run", "^TestResearchExhausted", "./core/transfer/lanes"],
                                    cwd=snapshot, env=dict(os.environ, GOWORK="off", GOTOOLCHAIN="local"),
                                    stdout=stream, stderr=subprocess.STDOUT)
        rows.append({"variant": name, "exitCode": result.returncode,
                     "log": str(log.relative_to(ROOT)), "sha256": hashlib.sha256(log.read_bytes()).hexdigest()})
        print(json.dumps(rows[-1]), flush=True)
    (output / "results.json").write_text(json.dumps(rows, indent=2) + "\n")
    assert rows[0]["exitCode"] != 0, "Expected the current source to expose the permanent-wait defect"
    assert rows[1]["exitCode"] == 0, "Single-variable classification control did not resolve the wait"


if __name__ == "__main__":
    main()
