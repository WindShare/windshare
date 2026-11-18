"""Check the actual Linux distribution on an x86-64 CPU without optional extensions."""
import asyncio
import json
import os
from pathlib import Path
import sys
import tempfile
from zipfile import ZipFile


async def boot(binary, config):
    process = await asyncio.create_subprocess_exec(
        "qemu-x86_64", "-cpu", "qemu64", str(binary), "--config", str(config),
        stdout=asyncio.subprocess.PIPE, env={**os.environ, "TOKIO_WORKER_THREADS": "2"},
    )
    try:
        async with asyncio.timeout(30):
            async for line in process.stdout:
                if json.loads(line).get("event") == "listening":
                    break
            else:
                raise RuntimeError(f"relay exited before readiness ({await process.wait()})")
            process.terminate()
            if await process.wait() != 0:
                raise RuntimeError(f"relay shutdown failed ({process.returncode})")
    finally:
        if process.returncode is None:
            process.kill()
            await process.wait()


async def verify(assets, version):
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        binary = root / "wsrelay"
        with ZipFile(assets / f"windshare-{version}-linux-amd64.zip") as release:
            binary.write_bytes(release.read(f"windshare-{version}/wsrelay"))
        binary.chmod(0o700)
        config = root / "node.json"
        config.write_text(json.dumps({
            "listen": "127.0.0.1:0", "endpoint": "ws://127.0.0.1/v2/ws",
            "database": "relay.sqlite", "identity_file": "identity.json",
            "stun": {"udp_addresses": [], "admin_address": None},
            "node": {"services": {"relay": True, "stun": False}},
        }), encoding="utf-8")
        await boot(binary, config)
        # Opening an existing SQLite B-tree executes code skipped by --help and first startup.
        await boot(binary, config)


if __name__ == "__main__":
    asyncio.run(verify(Path(sys.argv[1]), sys.argv[2]))
