"""Exercise public CLI downloads on the macOS runner's actual filesystem."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile
import time

READINESS_SECONDS = 30
TRANSFER_SECONDS = 120
STOP_SECONDS = 10
POLL_SECONDS = 0.001
HASH_BLOCK_BYTES = 1 << 20
FIRST_FILE_BYTES = 8 << 20
TAIL_FILE_BYTES = 128 << 20
FRONTEND = "https://windshare.example"


class Processes:
    def __init__(self, evidence):
        self.evidence = evidence
        self.children = []

    def start(self, name, arguments):
        stdout = self.evidence / f"{name}.stdout"
        stderr = self.evidence / f"{name}.stderr"
        with stdout.open("wb") as out, stderr.open("wb") as err:
            child = subprocess.Popen(
                arguments, stdout=out, stderr=err, stdin=subprocess.DEVNULL,
                start_new_session=True,
            )
        self.children.append((child, stdout, stderr))
        return child, stdout

    def wait(self, child):
        code = child.wait(timeout=TRANSFER_SECONDS)
        if code != 0:
            raise RuntimeError(f"CLI process {child.pid} exited with {code}")

    def kill(self, child):
        os.killpg(child.pid, signal.SIGKILL)
        child.wait(timeout=STOP_SECONDS)

    def close(self):
        # Each invocation gets a separate group; retire its children before the
        # temporary database and output disappear, including on assertion failure.
        for child, _, _ in reversed(self.children):
            if child.poll() is None:
                self.kill(child)

    def print_failures(self):
        for child, stdout, stderr in self.children:
            print(f"process={child.pid} exit={child.poll()}", flush=True)
            for path in (stdout, stderr):
                print(f"{path.name}:\n{path.read_text(errors='replace')[-12000:]}", flush=True)


def await_value(child, read, description):
    deadline = time.monotonic() + READINESS_SECONDS
    while time.monotonic() < deadline:
        value = read()
        if value:
            return value
        if child.poll() is not None:
            raise RuntimeError(f"process exited before {description}")
        time.sleep(POLL_SECONDS)
    raise TimeoutError(f"timed out waiting for {description}")


def share(processes, wind, source, relay, name):
    child, output = processes.start(name, [
        wind, "share", str(source), "--front-url", FRONTEND, "--relay", relay,
    ])
    match = await_value(
        child,
        lambda: re.search(r"(?m)^Link: (\S+)$", output.read_text(errors="replace")),
        "share link",
    )
    return match.group(1)


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(HASH_BLOCK_BYTES), b""):
            result.update(block)
    return result.digest()


def write_payload(path, size):
    block = bytes(range(256)) * (HASH_BLOCK_BYTES // 256)
    with path.open("wb") as output:
        while size:
            data = block[:min(size, len(block))]
            output.write(data)
            size -= len(data)


def relay_ready(path):
    for line in path.read_text(errors="replace").splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if event.get("event") == "listening":
            return "http://" + event["address"]
    return None


def verify_downloads(processes, wind, relay_binary, root, evidence):
    with (evidence / "cli-filesystem.txt").open("w") as environment:
        subprocess.run(["df", str(root)], stdout=environment, check=True)
    relay, ready = processes.start("relay", [
        relay_binary, "--listen", "127.0.0.1:0", "--database", str(root / "relay.sqlite"),
        "--stun-udp", "127.0.0.1:0", "--stun-admin", "127.0.0.1:0",
    ])
    endpoint = await_value(relay, lambda: relay_ready(ready), "relay readiness")

    source = root / "single.bin"
    write_payload(source, HASH_BLOCK_BYTES)
    link = share(processes, wind, source, endpoint, "single-share")
    output = root / "single-output"
    receiver, _ = processes.start("single-get", [
        wind, "get", link, "-o", str(output), "--connectivity", "relay-only",
        "--trace", str(evidence / "single-get.ndjson"),
    ])
    processes.wait(receiver)
    assert digest(source) == digest(output / source.name), "single-file content mismatch"
    print("PASS single-file CLI download", flush=True)

    tree = root / "resume-tree"
    (tree / "nested" / "empty").mkdir(parents=True)
    write_payload(tree / "first.bin", FIRST_FILE_BYTES)
    write_payload(tree / "nested" / "tail.bin", TAIL_FILE_BYTES)
    link = share(processes, wind, tree, endpoint, "folder-share")
    output = root / "folder-output"
    receiver, _ = processes.start("interrupted-get", [
        wind, "get", link, "-o", str(output), "--connectivity", "relay-only",
        "--trace", str(evidence / "interrupted-get.ndjson"),
    ])
    first = output / tree.name / "first.bin"
    await_value(receiver, lambda: first.is_file() and first.stat().st_size == FIRST_FILE_BYTES,
                "first published file before receiver crash")
    assert receiver.poll() is None, "receiver completed before the crash"
    processes.kill(receiver)

    inventory, inventory_output = processes.start("resume-list", [
        wind, "resume", "list", "-o", str(output),
    ])
    processes.wait(inventory)
    inventory_text = inventory_output.read_text()
    assert 'operations=1 registry_unknown=false' in inventory_text, inventory_text
    assert 'state="resumable"' in inventory_text or 'state="incomplete"' in inventory_text, inventory_text

    resumed, _ = processes.start("resume-retry", [
        wind, "resume", "retry", link, "-o", str(output), "--connectivity", "relay-only",
        "--trace", str(evidence / "resumed.ndjson"),
    ])
    processes.wait(resumed)
    for relative in (Path("first.bin"), Path("nested/tail.bin")):
        assert digest(tree / relative) == digest(output / tree.name / relative), relative
    assert (output / tree.name / "nested/empty").is_dir(), "empty directory was lost"
    print("PASS folder CLI download, SIGKILL recovery, and resume retry", flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--wind", required=True)
    parser.add_argument("--relay", required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    arguments = parser.parse_args()
    arguments.evidence.mkdir(parents=True, exist_ok=True)
    processes = Processes(arguments.evidence)
    with tempfile.TemporaryDirectory(prefix="windshare-native-cli-") as temporary:
        try:
            verify_downloads(processes, arguments.wind, arguments.relay,
                             Path(temporary), arguments.evidence)
        except BaseException:
            processes.print_failures()
            raise
        finally:
            processes.close()


if __name__ == "__main__":
    main()
