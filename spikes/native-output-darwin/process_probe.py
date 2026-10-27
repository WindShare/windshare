"""Separate writer/reader processes expose exact SIGKILL cuts to the controller."""

import fcntl
import hashlib
import json
import os
import signal
import sys

from native import (
    identity, install_record, open_directory, open_file, write_at, write_record,
)

CHUNK_BYTES = 64 * 1024
FILE_BYTES = 2 * 1024 * 1024
REVISION = "native-output-probe-revision-v1"
CHECKPOINT_VERSION = 1
BLOCKED_EXIT = 3
BUSY_EXIT = 4


def payload(offset, size):
    # Different offsets have different bytes, so a misplaced range cannot pass.
    seed = hashlib.sha256(str(offset // CHUNK_BYTES).encode()).digest()
    return (seed * ((size + len(seed) - 1) // len(seed)))[:size]


def announce(value):
    print(json.dumps(value, sort_keys=True), flush=True)


def wait_for_kill(cut, requested):
    if cut == requested:
        announce({"cut": cut, "pid": os.getpid()})
        while True:
            signal.pause()


def create_checkpoint(root, fd, ranges):
    return {
        "version": CHECKPOINT_VERSION, "revision": REVISION,
        "root_identity": identity(root), "file_identity": identity(fd),
        "size": FILE_BYTES, "ranges": ranges,
    }


def produce(path, cut):
    root = open_directory(path)
    fd = open_file(root, "partial", os.O_CREAT | os.O_EXCL | os.O_RDWR)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        os.ftruncate(fd, FILE_BYTES)
        write_at(fd, payload(0, CHUNK_BYTES))
        os.fsync(fd)
        record = create_checkpoint(root, fd, [[0, CHUNK_BYTES]])
        write_record(root, record)
        install_record(root)

        offset = 2 * CHUNK_BYTES
        write_at(fd, payload(offset, CHUNK_BYTES), offset)
        os.fsync(fd)
        wait_for_kill("data-synced", cut)

        record["ranges"].append([offset, offset + CHUNK_BYTES])
        write_record(root, record)
        wait_for_kill("checkpoint-staged", cut)
        install_record(root)
        wait_for_kill("checkpoint-installed", cut)

        for start in range(0, FILE_BYTES, CHUNK_BYTES):
            if start not in (0, offset):
                write_at(fd, payload(start, CHUNK_BYTES), start)
        os.fsync(fd)
        record["ranges"] = [[0, FILE_BYTES]]
        write_record(root, record)
        install_record(root)
        os.link("partial", "final", src_dir_fd=root, dst_dir_fd=root, follow_symlinks=False)
        os.fsync(root)
        wait_for_kill("published-before-settlement", cut)
        raise ValueError("unknown crash cut")
    finally:
        os.close(fd)
        os.close(root)


def recover(path):
    root = open_directory(path)
    fd = None
    try:
        record_fd = open_file(root, "checkpoint.json", os.O_RDONLY)
        with os.fdopen(record_fd, "r") as source:
            record = json.load(source)
        if record["version"] != CHECKPOINT_VERSION or record["revision"] != REVISION:
            return {"blocked": "revision-or-schema"}
        if identity(root) != record["root_identity"]:
            return {"blocked": "root-replaced"}
        fd = open_file(root, "partial")
        if identity(fd) != record["file_identity"] or os.fstat(fd).st_size != record["size"]:
            return {"blocked": "file-replaced"}
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        covered = set()
        for start, end in record["ranges"]:
            if start % CHUNK_BYTES or end % CHUNK_BYTES or not 0 <= start < end <= FILE_BYTES:
                return {"blocked": "invalid-range"}
            for offset in range(start, end, CHUNK_BYTES):
                if offset in covered:
                    return {"blocked": "overlapping-range"}
                # This assertion checks experimental integrity; it is not a
                # proposal to reread all downloaded bytes on production resume.
                if os.pread(fd, CHUNK_BYTES, offset) != payload(offset, CHUNK_BYTES):
                    return {"blocked": "recorded-bytes-changed"}
                covered.add(offset)
        for offset in range(0, FILE_BYTES, CHUNK_BYTES):
            if offset not in covered:
                write_at(fd, payload(offset, CHUNK_BYTES), offset)
        os.fsync(fd)
        try:
            os.link("partial", "final", src_dir_fd=root, dst_dir_fd=root, follow_symlinks=False)
        except FileExistsError:
            final_fd = open_file(root, "final", os.O_RDONLY)
            try:
                if identity(final_fd) != record["file_identity"]:
                    return {"blocked": "foreign-final"}
            finally:
                os.close(final_fd)
        os.fsync(root)
        final_fd = open_file(root, "final", os.O_RDONLY)
        try:
            same = identity(final_fd) == record["file_identity"]
            digest = hashlib.sha256(os.pread(final_fd, FILE_BYTES, 0)).hexdigest()
        finally:
            os.close(final_fd)
        return {
            "same_object": same, "reused_bytes": len(covered) * CHUNK_BYTES,
            "written_bytes": FILE_BYTES - len(covered) * CHUNK_BYTES,
            "sha256": digest, "recovery_pid": os.getpid(),
        }
    finally:
        if fd is not None:
            os.close(fd)
        os.close(root)


def lock_probe(path, hold):
    root = open_directory(path)
    fd = open_file(root, "lock", os.O_CREAT | os.O_RDWR)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            announce({"locked": False})
            return BUSY_EXIT
        announce({"locked": True, "pid": os.getpid()})
        if hold:
            while True:
                signal.pause()
        return 0
    finally:
        os.close(fd)
        os.close(root)


if __name__ == "__main__":
    command, directory = sys.argv[1:3]
    if command == "produce":
        produce(directory, sys.argv[3])
    elif command == "recover":
        result = recover(directory)
        announce(result)
        sys.exit(BLOCKED_EXIT if "blocked" in result else 0)
    elif command in ("lock", "hold-lock"):
        sys.exit(lock_probe(directory, command == "hold-lock"))
    else:
        raise ValueError("unknown helper mode")
