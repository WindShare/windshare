"""Native Linux cross-host or same-host control; Windows handles bootstrap only."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import time
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from experiment_paths import experiment_directory
spec = importlib.util.spec_from_file_location("native_lab", ROOT / "testdata/connectivity/webrtc/adaptive-20261023/cross-host.py")
lab = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lab)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--build", required=True)
    parser.add_argument("--public-endpoint", required=True)
    parser.add_argument("--sender", choices=["lan", "public"], required=True)
    parser.add_argument("--size-mib", type=int, default=64)
    parser.add_argument("--repeats", type=int, default=1)
    parser.add_argument("--deadline-seconds", type=int, default=180)
    parser.add_argument("--lan-interface", default="")
    parser.add_argument("--same-host", action="store_true")
    args = parser.parse_args()
    build = experiment_directory(ROOT, args.build)
    study = build / f"linux-cross-{args.sender}-{time.time_ns()}"
    study.mkdir()
    fixture = (build / "fixture.bin").read_bytes()[:args.size_mib * 1048576]
    assert len(fixture) == args.size_mib * 1048576
    (study / "probe.bin").write_bytes(fixture)
    (study / "config.json").write_text('{"profiles":[],"active_relays":[]}')
    report = {"sender": args.sender, "sameHost": args.same_host, "endpoint": "lan" if args.same_host else "public", "lanInterface": args.lan_interface, "size": len(fixture), "sha256": hashlib.sha256(fixture).hexdigest(),
              "linuxSHA256": hashlib.sha256((build / "binaries/wind-linux").read_bytes()).hexdigest(),
              "scope": "Native Linux CLI; same-host control." if args.same_host else "Linux LAN/public CLI; Windows/SSH carry signaling only; p2p-only content.",
              "rounds": []}
    report_path = study / "report.json"

    def save():
        report_path.write_text(json.dumps(report, indent=2) + "\n")

    clients, directories, processes = {}, {}, []
    save()
    try:
        for endpoint in ["lan", "public"]:
            clients[endpoint] = lab.connect("lan" if endpoint == "lan" or args.same_host else args.public_endpoint)
            directory = lab.remote_command(clients[endpoint], "import tempfile; print(tempfile.mkdtemp(prefix='windshare-wan-'))").strip()
            directories[endpoint] = directory
            with clients[endpoint].open_sftp() as sftp:
                sftp.put(str(build / "binaries/wind-linux"), directory + "/wind")
                sftp.chmod(directory + "/wind", 0o700)
                sftp.put(str(study / "config.json"), directory + "/config.json")
                if endpoint == args.sender:
                    sftp.put(str(study / "probe.bin"), directory + "/probe.bin")
            identity = lab.remote_command(clients[endpoint],
                "import hashlib; print(hashlib.sha256(open(" + repr(directory + "/wind") + ",'rb').read()).hexdigest())").strip()
            assert identity == report["linuxSHA256"]
        relay = lab.Process([str(build / "binaries/relay-native.exe"), "--listen", "127.0.0.1:0", "--database",
            str(study / "relay.sqlite"), "--origins", "http://127.0.0.1:9999",
            "--stun-udp", "127.0.0.1:0", "--stun-admin", "127.0.0.1:0"], ROOT)
        processes.append(relay)

        def relay_ready():
            for line in relay.text().splitlines():
                try:
                    item = json.loads(line)
                    if item.get("event") == "listening":
                        return item["endpoint"]
                except ValueError:
                    pass

        port = urlparse(lab.until(relay_ready, relay)).port
        # Two clients on one host share the same forwarded bootstrap listener.
        for client in ([clients["lan"]] if args.same_host else clients.values()):
            lab.forward(client.get_transport(), port)
        relay_url = "ws://127.0.0.1:" + str(port)
        sender_endpoint = args.sender
        receiver_endpoint = "lan" if sender_endpoint == "public" else "public"
        sender_root, receiver_root = directories[sender_endpoint], directories[receiver_endpoint]
        for repetition in range(args.repeats):
            row = {"repetition": repetition + 1,
                   "udpCountersBefore": {endpoint: lab.linux_udp_counters(client) for endpoint, client in clients.items()}}
            report["rounds"].append(row)
            sender = lab.Process([sender_root + "/wind", "share", sender_root + "/probe.bin", "--relay", relay_url,
                "--front-url", "http://127.0.0.1:9999", "--block-size", "1048576", "--split-key",
                "--trace", sender_root + f"/sender-{repetition}.ndjson"], sender_root,
                {"WINDSHARE_CONFIG": sender_root + "/config.json",
                 "WS_WAN_INTERFACE": args.lan_interface if sender_endpoint == "lan" or args.same_host else ""}, clients[sender_endpoint])
            processes.append(sender)
            lab.until(lambda: "Key: " in sender.text() and "Bare link: " in sender.text(), sender)
            link = next(line[11:] for line in sender.text().splitlines() if line.startswith("Bare link: "))
            key = next(line[5:] for line in sender.text().splitlines() if line.startswith("Key: "))
            output = receiver_root + f"/output-{repetition}"
            lab.remote_command(clients[receiver_endpoint], "import os; os.mkdir(" + repr(output) + ")")
            started = time.monotonic()
            row["receiverStartedMs"] = time.time() * 1000
            receiver = lab.Process([receiver_root + "/wind", "get", link, "--key", key, "-o", output,
                "--connectivity", "p2p-only", "--wait-timeout", "35s",
                "--trace", receiver_root + f"/receiver-{repetition}.ndjson"], receiver_root,
                {"WINDSHARE_CONFIG": receiver_root + "/config.json",
                 "WS_WAN_INTERFACE": args.lan_interface if receiver_endpoint == "lan" or args.same_host else ""}, clients[receiver_endpoint])
            processes.append(receiver)
            deadline = started + args.deadline_seconds
            while receiver.poll() is None and time.monotonic() < deadline:
                time.sleep(.02)
            row.update(completionMs=(time.monotonic()-started)*1000, exitCode=receiver.poll())
            row["deadlineExceeded"] = row["exitCode"] is None
            receiver.close()
            sender.close()
            row["udpCountersAfter"] = {endpoint: lab.linux_udp_counters(client) for endpoint, client in clients.items()}
            for endpoint, role, process in [(sender_endpoint, "sender", sender), (receiver_endpoint, "receiver", receiver)]:
                (study / f"{role}-{repetition}.stderr").write_bytes(process.errors)
                with clients[endpoint].open_sftp() as sftp:
                    sftp.get(directories[endpoint] + f"/{role}-{repetition}.ndjson", str(study / f"{role}-{repetition}.ndjson"))
                    for filename in sftp.listdir(directories[endpoint]):
                        if filename.startswith("research-"):
                            sftp.get(directories[endpoint] + "/" + filename, str(study / filename))
            if row["exitCode"] == 0:
                row["sha256"] = lab.remote_command(clients[receiver_endpoint],
                    "import hashlib; print(hashlib.sha256(open(" + repr(output + "/probe.bin") + ",'rb').read()).hexdigest())").strip()
                assert row["sha256"] == report["sha256"]
                row["MBpsIncludingStartup"] = len(fixture) / row["completionMs"] / 1000
            save()
            print(json.dumps(row), flush=True)
        report["status"] = "passed" if all(row["exitCode"] == 0 for row in report["rounds"]) else "failed"
    except Exception as error:
        report.update(status="failed", error=str(error))
        raise
    finally:
        for process in reversed(processes):
            process.close()
        cleanup = {}
        for endpoint, directory in directories.items():
            try:
                lab.remote_command(clients[endpoint],
                    "import pathlib,shutil; p=pathlib.Path(" + repr(directory) +
                    "); assert p.parent==pathlib.Path('/tmp') and p.name.startswith('windshare-wan-'); shutil.rmtree(p)")
                cleanup[endpoint] = "complete"
            except Exception as error:
                cleanup[endpoint] = str(error)
        report["remoteCleanup"] = cleanup
        save()
        for client in clients.values():
            client.close()
        print(str(report_path), flush=True)


if __name__ == "__main__":
    main()
