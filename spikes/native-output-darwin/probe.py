"""Run bounded native experiments and retain structured evidence for each case."""

import argparse
import concurrent.futures
import errno
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import traceback

from native import (
    DIRECTORY_FLAGS, FULL_FSYNC, identity, mutation_observation, open_directory,
    open_file, rename_exclusive, volume_capabilities, volume_uuid, write_at,
)
from process_probe import BLOCKED_EXIT, BUSY_EXIT, CHUNK_BYTES, FILE_BYTES, payload

PROCESS_TIMEOUT_SECONDS = 15
CONTENDERS = 12
HELPER = Path(__file__).with_name("process_probe.py")


def require(condition, reason):
    if not condition:
        raise AssertionError(reason)


def run_child(mode, path, expected_exit=0):
    result = subprocess.run(
        [sys.executable, str(HELPER), mode, str(path)],
        capture_output=True, text=True, timeout=PROCESS_TIMEOUT_SECONDS,
    )
    require(result.returncode == expected_exit, result.stdout + result.stderr)
    return json.loads(result.stdout)


def start_waiting(mode, path, *args):
    child = subprocess.Popen(
        [sys.executable, str(HELPER), mode, str(path), *args],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(child.stdout, selectors.EVENT_READ)
            require(bool(selector.select(PROCESS_TIMEOUT_SECONDS)), "helper did not reach its cut")
        line = child.stdout.readline()
        if not line:
            _, error = child.communicate(timeout=PROCESS_TIMEOUT_SECONDS)
            raise AssertionError("helper exited before its cut: " + error)
        return child, json.loads(line)
    except BaseException:
        stop_child(child)
        raise


def stop_child(child):
    if child.poll() is None:
        child.kill()
    child.communicate(timeout=PROCESS_TIMEOUT_SECONDS)


def killed_writer(path, cut):
    child, ready = start_waiting("produce", path, cut)
    try:
        require(ready["cut"] == cut, "wrong writer cut")
        child.kill()
        child.wait(timeout=PROCESS_TIMEOUT_SECONDS)
        require(child.returncode == -signal.SIGKILL, "writer did not die by SIGKILL")
    finally:
        stop_child(child)
    return ready["pid"]


def environment(path):
    root = open_directory(path)
    try:
        raw = subprocess.check_output(
            ["diskutil", "info", "-plist", str(path)], timeout=PROCESS_TIMEOUT_SECONDS,
        )
        disk = plistlib.loads(raw)
        return {
            "system": platform.platform(), "architecture": platform.machine(),
            "python": sys.version, "runner_image": os.environ.get("ImageOS"),
            "runner_image_version": os.environ.get("ImageVersion"),
            "filesystem": disk.get("FilesystemType"),
            "filesystem_personality": disk.get("FilesystemName"),
            "volume_uuid": volume_uuid(root), "root_identity": identity(root),
            "capabilities": volume_capabilities(root),
        }
    finally:
        os.close(root)


def identity_and_publication(path):
    root = open_directory(path)
    fd = open_file(root, "stage", os.O_CREAT | os.O_EXCL | os.O_RDWR)
    try:
        write_at(fd, b"authenticated test bytes")
        os.fsync(fd)
        original = identity(fd)
        before = os.fstat(fd)
        os.link("stage", "linked", src_dir_fd=root, dst_dir_fd=root, follow_symlinks=False)
        linked = open_file(root, "linked")
        try:
            require(identity(linked) == original, "hard link changed object identity")
            require(os.fstat(linked).st_nlink == 2, "hard link did not retain one inode")
        finally:
            os.close(linked)
        rename_exclusive(root, "stage", "final")
        final = open_file(root, "final")
        try:
            after = os.fstat(final)
            require(identity(final) == original, "rename copied/replaced the object")
            require(before.st_blocks == after.st_blocks, "publication changed data allocation")
            os.fsync(root)
            full_sync = None
            try:
                fcntl.fcntl(final, FULL_FSYNC)
            except OSError as error:
                full_sync = error.errno
            return {
                "identity": original, "blocks_before": before.st_blocks,
                "blocks_after": after.st_blocks, "links": after.st_nlink,
                "file_and_directory_fsync": True, "full_fsync_errno": full_sync,
                "mutation": mutation_observation(final),
            }
        finally:
            os.close(final)
    finally:
        os.close(fd)
        os.close(root)


def publication_collisions(path):
    root = open_directory(path)
    try:
        kinds = ("file", "directory", "symlink")
        for kind in kinds:
            stage = open_file(root, "stage", os.O_CREAT | os.O_EXCL | os.O_RDWR)
            try:
                write_at(stage, b"downloaded")
                saved = identity(stage)
                if kind == "file":
                    (path / "final").write_bytes(b"foreign")
                elif kind == "directory":
                    (path / "final").mkdir()
                else:
                    os.symlink("elsewhere", "final", dir_fd=root)
                foreign = os.stat("final", dir_fd=root, follow_symlinks=False)
                try:
                    rename_exclusive(root, "stage", "final")
                    raise AssertionError("exclusive rename replaced a collision")
                except OSError as error:
                    require(error.errno in (errno.EEXIST, errno.ELOOP), str(error))
                require(identity(stage) == saved, "collision invalidated the live stage")
                require(os.pread(stage, 10, 0) == b"downloaded", "collision lost downloaded bytes")
                current = os.stat("final", dir_fd=root, follow_symlinks=False)
                require(current.st_ino == foreign.st_ino, "collision changed the foreign object")
                if kind == "file":
                    require((path / "final").read_bytes() == b"foreign", "foreign bytes changed")
                if kind == "directory":
                    os.rmdir("final", dir_fd=root)
                else:
                    os.unlink("final", dir_fd=root)
                rename_exclusive(root, "stage", "final")
                reopened = open_file(root, "final")
                try:
                    require(identity(reopened) == saved, "retry did not publish the retained stage")
                finally:
                    os.close(reopened)
                os.unlink("final", dir_fd=root)
            finally:
                os.close(stage)

        os.mkdir("candidate", dir_fd=root)
        os.mkdir("occupied", dir_fd=root)
        try:
            rename_exclusive(root, "candidate", "occupied")
            raise AssertionError("directory reservation replaced an existing directory")
        except FileExistsError:
            pass
        rename_exclusive(root, "candidate", "reserved")
        return {"collision_kinds": list(kinds), "retry_reused_stage": True, "directory_no_replace": True}
    finally:
        os.close(root)


def concurrent_publication(path):
    root = open_directory(path)
    try:
        identities = {}
        for index in range(CONTENDERS):
            name = f"stage-{index}"
            fd = open_file(root, name, os.O_CREAT | os.O_EXCL | os.O_RDWR)
            try:
                write_at(fd, name.encode())
                identities[name] = identity(fd)
            finally:
                os.close(fd)

        def publish(name):
            try:
                rename_exclusive(root, name, "winner")
                return name
            except FileExistsError:
                return None

        with concurrent.futures.ThreadPoolExecutor(max_workers=CONTENDERS) as pool:
            winners = [name for name in pool.map(publish, identities) if name is not None]
        require(len(winners) == 1, "publication did not select exactly one winner")
        winner = open_file(root, "winner")
        try:
            require(identity(winner) == identities[winners[0]], "winner identity mismatch")
        finally:
            os.close(winner)
        require(len(list(path.glob("stage-*"))) == CONTENDERS - 1, "losing stages were lost")
        return {"contenders": CONTENDERS, "winners": len(winners), "losers_retained": CONTENDERS - 1}
    finally:
        os.close(root)


def placement_and_names(path):
    destination = path / "destination"
    destination.mkdir()
    root = open_directory(destination)
    original = identity(root)
    try:
        os.rename(destination, path / "moved")
        destination.mkdir()
        fd = open_file(root, "held-handle", os.O_CREAT | os.O_EXCL | os.O_RDWR)
        os.close(fd)
        fresh = open_directory(destination)
        try:
            require(identity(fresh) != original, "root replacement was not detected")
        finally:
            os.close(fresh)
        require((path / "moved" / "held-handle").exists(), "retained handle lost its old root")
        require(not (destination / "held-handle").exists(), "retained handle followed replacement")
        os.symlink(str(destination), "redirect", dir_fd=root)
        try:
            redirected = os.open("redirect", DIRECTORY_FLAGS, dir_fd=root)
        except OSError as error:
            symlink_errno = error.errno
        else:
            os.close(redirected)
            raise AssertionError("O_NOFOLLOW accepted a symlink directory")
        fd = open_file(root, "CaseName", os.O_CREAT | os.O_EXCL | os.O_RDWR)
        os.close(fd)
        observed_case_sensitive = not (path / "moved" / "casename").exists()
        caps = volume_capabilities(root)["case_sensitive"]
        require(caps["valid"] and caps["supported"] == observed_case_sensitive, "case capability mismatch")
        return {
            "root_replacement_detected": True, "old_handle_stays_on_old_root": True,
            "symlink_errno": symlink_errno, "case_sensitive": observed_case_sensitive,
        }
    finally:
        os.close(root)


def locking(path):
    holder, ready = start_waiting("hold-lock", path)
    try:
        require(ready["locked"], "holder did not acquire lock")
        require(not run_child("lock", path, BUSY_EXIT)["locked"], "second process acquired a held lock")
        holder.kill()
        holder.wait(timeout=PROCESS_TIMEOUT_SECONDS)
        require(holder.returncode == -signal.SIGKILL, "lock holder did not die by SIGKILL")
        require(run_child("lock", path)["locked"], "lock was not released on process death")
        return {"second_process_blocked": True, "released_after_sigkill": True}
    finally:
        stop_child(holder)


def restart(path, cut):
    writer = killed_writer(path, cut)
    result = run_child("recover", path)
    expected = {
        "data-synced": CHUNK_BYTES, "checkpoint-staged": CHUNK_BYTES,
        "checkpoint-installed": 2 * CHUNK_BYTES, "published-before-settlement": FILE_BYTES,
    }[cut]
    expected_hash = hashlib.sha256(b"".join(
        payload(offset, CHUNK_BYTES) for offset in range(0, FILE_BYTES, CHUNK_BYTES)
    )).hexdigest()
    require(result["writer_pid"] != writer, "recovery reused writer process state")
    require(result["reused_bytes"] == expected, "recovery inferred progress beyond the installed checkpoint")
    require(result["written_bytes"] == FILE_BYTES - expected, "recovery rewrote verified ranges")
    require(result["same_object"] and result["sha256"] == expected_hash, "recovery changed identity or bytes")
    return {"cut": cut, "killed_pid": writer, **result}


def reject_replacement(path, kind):
    killed_writer(path, "checkpoint-installed")
    if kind == "root":
        moved = path.with_name(path.name + "-old")
        os.rename(path, moved)
        try:
            shutil.copytree(moved, path)
        finally:
            shutil.rmtree(moved)
        reason = "root-replaced"
    elif kind == "file":
        shutil.copy2(path / "partial", path / "replacement")
        os.replace(path / "replacement", path / "partial")
        reason = "file-replaced"
    elif kind == "revision":
        record = json.loads((path / "checkpoint.json").read_text())
        record["revision"] = "different-source-revision"
        (path / "checkpoint.json").write_text(json.dumps(record))
        reason = "revision-or-schema"
    else:
        with (path / "partial").open("r+b") as partial:
            partial.write(b"changed")
        reason = "recorded-bytes-changed"
    before = (path / "partial").read_bytes()
    result = run_child("recover", path, BLOCKED_EXIT)
    require(result["blocked"] == reason, "wrong rejection reason")
    require((path / "partial").read_bytes() == before, "rejected recovery mutated the object")
    require(not (path / "final").exists(), "rejected recovery published an output")
    return {"rejected": reason, "existing_bytes_untouched": True}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.root.mkdir(parents=True, exist_ok=True)
    report = {"scope": "native primitives and process death; not power loss or product certification",
              "environment": environment(args.root), "cases": []}
    require(report["environment"]["filesystem"] == "apfs", "experiment requires an observed APFS volume")
    cases = [
        ("identity-publication", identity_and_publication),
        ("publication-collisions", publication_collisions),
        ("concurrent-publication", concurrent_publication),
        ("placement-and-names", placement_and_names),
        ("process-lock", locking),
    ]
    for cut in ("data-synced", "checkpoint-staged", "checkpoint-installed", "published-before-settlement"):
        cases.append(("restart-" + cut, lambda path, cut=cut: restart(path, cut)))
    for kind in ("file", "root", "revision", "content"):
        cases.append(("reject-" + kind, lambda path, kind=kind: reject_replacement(path, kind)))
    for name, experiment in cases:
        started = time.monotonic()
        case = {"operation": name}
        try:
            with tempfile.TemporaryDirectory(prefix=name + "-", dir=args.root) as directory:
                case.update(status="passed", evidence=experiment(Path(directory)))
        except Exception:
            case.update(status="failed", error=traceback.format_exc())
        case["elapsed_seconds"] = round(time.monotonic() - started, 3)
        report["cases"].append(case)
        print(json.dumps(case, sort_keys=True), flush=True)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    return int(any(case["status"] != "passed" for case in report["cases"]))


if __name__ == "__main__":
    sys.exit(main())
