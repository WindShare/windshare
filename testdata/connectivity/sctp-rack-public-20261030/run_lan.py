"""Pair Windows senders against one unchanged upstream Linux receiver per run."""
import argparse
import json
from pathlib import Path, PurePosixPath
import shlex
import subprocess

BASE_PORT = 36470
ROUNDS = 3
CLIENT_TIMEOUT_SECONDS = 12
RECEIVER_TIMEOUT_SECONDS = 14


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ssh-host", required=True, help="receiver SSH alias or user@host")
    parser.add_argument("--address", required=True, help="receiver LAN address")
    parser.add_argument("--remote-dir", required=True, help="absolute POSIX path to the receiver checkout")
    parser.add_argument("--output", required=True, help="new local output directory")
    args = parser.parse_args()
    remote = PurePosixPath(args.remote_dir)
    if not remote.is_absolute():
        parser.error("--remote-dir must be absolute")
    out = Path(args.output).resolve()
    out.mkdir(parents=True, exist_ok=False)
    root = Path(__file__).resolve().parent
    remote_output = remote / "results" / out.name
    ssh = ["ssh", "-o", "BatchMode=yes", args.ssh_host]
    subprocess.run([*ssh, shlex.join(["mkdir", "-p", str(remote_output)])], check=True)
    records = []
    for number in range(1, ROUNDS + 1):
        variants = ("upstream", "repaired") if number % 2 else ("repaired", "upstream")
        for variant in variants:
            run_id = f"double-loss-{number}-{variant}"
            port = BASE_PORT + len(records)
            server_args = [str(remote / ".build/upstream-linux"), "-listen", f"0.0.0.0:{port}",
                           "-timeout", f"{CLIENT_TIMEOUT_SECONDS}s",
                           "-trace", str(remote_output / f"{run_id}-receiver.jsonl")]
            server = subprocess.Popen([*ssh, shlex.join(server_args)],
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                ready = server.stdout.readline()
                if not ready or "ready" not in json.loads(ready):
                    raise RuntimeError(f"Receiver not ready: {ready} {server.stderr.read()}")
                client_args = [str(root / ".build" / f"{variant}.exe"),
                               "-remote", f"{args.address}:{port}", "-trace", str(out / f"{run_id}.jsonl")]
                result = subprocess.run(client_args, capture_output=True, text=True,
                                        timeout=CLIENT_TIMEOUT_SECONDS)
                _, server_error = server.communicate(timeout=RECEIVER_TIMEOUT_SECONDS)
                record = {"id": run_id, "case": "double-loss", "variant": variant,
                          "exit_code": result.returncode, "receiver_exit_code": server.returncode,
                          "command": client_args, "receiver_command": server_args,
                          "stderr": result.stderr, "receiver_stderr": server_error}
                if result.stdout.strip():
                    record["summary"] = json.loads(result.stdout.strip().splitlines()[-1])
                records.append(record)
                (out / "results.json").write_text(json.dumps(records, indent=2) + "\n", encoding="utf8")
                summary = record.get("summary", {})
                print(f"{run_id}: client={result.returncode}, receiver={server.returncode}, "
                      f"T3={summary.get('sender_t3_timeouts')}, "
                      f"recovery={summary.get('last_drop_to_recovery_ms')}", flush=True)
            finally:
                if server.poll() is None:
                    server.terminate()
                    server.wait(timeout=5)
    if any(record["exit_code"] or record["receiver_exit_code"] for record in records):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
