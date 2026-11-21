import hashlib
import io
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import unittest


INSTALL_SCRIPT = Path(__file__).resolve().with_name("install-web.sh")
HEREDOC_PATTERN = re.compile(r"<<'PY'\r?\n(.*?)\r?\nPY(?:\r?\n|$)", re.DOTALL)


def load_embedded_python_blocks() -> tuple[str, str, str]:
    blocks = HEREDOC_PATTERN.findall(INSTALL_SCRIPT.read_text(encoding="utf-8"))
    if len(blocks) != 3:
        raise AssertionError(f"Expected 3 embedded Python blocks in install-web.sh, found {len(blocks)}")
    return blocks[0], blocks[1], blocks[2]


EXTRACT_BLOCK, PUBLISH_BLOCK, PRUNE_BLOCK = load_embedded_python_blocks()


def run_block(code: str, *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-c", code, *args],
        check=False,
        capture_output=True,
        text=True,
    )


def make_archive(path: Path, entries: dict[str, bytes]) -> str:
    with tarfile.open(path, "w:gz") as archive:
        for name, data in entries.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            archive.addfile(info, io.BytesIO(data))
    return hashlib.sha256(path.read_bytes()).hexdigest()


def release_dirname(tag_index: int, sha256: str) -> str:
    return f"{tag_index:040x}-{sha256}"


class InstallWebDownloadsTests(unittest.TestCase):
    def _install_release(
        self,
        root: Path,
        *,
        tag_index: int,
        version: str | None,
        assets: dict[str, bytes] | None = None,
        retain: int = 3,
        mtime_ns: int,
    ) -> tuple[Path, tuple[int, int, int]]:
        releases = root / "releases"
        downloads = root / "downloads"
        stage = root / f".downloads-stage-{tag_index}"
        releases.mkdir(exist_ok=True)
        downloads.mkdir(exist_ok=True)
        stage.mkdir(exist_ok=True)

        entries: dict[str, bytes] = {"web/index.html": f"<html>{tag_index}</html>".encode()}
        if version is not None:
            for name, content in (assets or {}).items():
                entries[f"downloads/{version}/{name}"] = content

        archive_path = root / f"artifact-{tag_index}.tar.gz"
        digest = make_archive(archive_path, entries)
        candidate = releases / f".stage-{tag_index}"
        candidate.mkdir()

        extracted = run_block(EXTRACT_BLOCK, str(archive_path), str(candidate))
        self.assertEqual(extracted.returncode, 0, extracted.stderr)

        destination = releases / release_dirname(tag_index, digest)
        if not destination.exists():
            candidate.rename(destination)
        os.utime(destination, ns=(mtime_ns, mtime_ns))

        published = run_block(
            PUBLISH_BLOCK,
            str(releases),
            str(destination),
            str(downloads),
            str(stage),
            str(retain),
        )
        self.assertEqual(published.returncode, 0, published.stderr)

        pruned = run_block(
            PRUNE_BLOCK,
            str(releases),
            str(destination),
            str(downloads),
            str(retain),
        )
        self.assertEqual(pruned.returncode, 0, pruned.stderr)
        counts = tuple(int(part) for part in pruned.stdout.strip().split())
        self.assertEqual(len(counts), 3)
        return destination, counts

    def test_retains_versioned_downloads_across_release_switches_and_prunes_stale(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            r1, c1 = self._install_release(
                root,
                tag_index=1,
                version="v0.1.3",
                assets={"windshare-v0.1.3-linux-amd64.zip": b"pkg-013"},
                mtime_ns=1_000_000_000,
            )
            self.assertEqual(c1, (0, 1, 0))

            r2, c2 = self._install_release(
                root,
                tag_index=2,
                version="v0.1.4",
                assets={"windshare-v0.1.4-linux-amd64.zip": b"pkg-014"},
                mtime_ns=2_000_000_000,
            )
            self.assertEqual(c2, (0, 2, 0))
            self.assertTrue((root / "downloads/v0.1.3/windshare-v0.1.3-linux-amd64.zip").is_file())
            self.assertTrue((root / "downloads/v0.1.4/windshare-v0.1.4-linux-amd64.zip").is_file())
            self.assertTrue(
                os.path.samefile(
                    r1 / "downloads/v0.1.3/windshare-v0.1.3-linux-amd64.zip",
                    root / "downloads/v0.1.3/windshare-v0.1.3-linux-amd64.zip",
                )
            )
            self.assertTrue(
                os.path.samefile(
                    r2 / "downloads/v0.1.4/windshare-v0.1.4-linux-amd64.zip",
                    root / "downloads/v0.1.4/windshare-v0.1.4-linux-amd64.zip",
                )
            )

            _, c3 = self._install_release(
                root,
                tag_index=3,
                version="v0.1.5",
                assets={"windshare-v0.1.5-linux-amd64.zip": b"pkg-015"},
                mtime_ns=3_000_000_000,
            )
            self.assertEqual(c3, (0, 3, 0))

            _, c4 = self._install_release(
                root,
                tag_index=4,
                version="v0.1.6",
                assets={"windshare-v0.1.6-linux-amd64.zip": b"pkg-016"},
                mtime_ns=4_000_000_000,
            )
            self.assertEqual(c4, (1, 3, 1))
            self.assertFalse(r1.exists())
            self.assertFalse((root / "downloads/v0.1.3").exists())
            self.assertEqual(
                sorted(entry.name for entry in (root / "downloads").iterdir()),
                ["v0.1.4", "v0.1.5", "v0.1.6"],
            )

    def test_backfills_preexisting_releases_and_updates_republished_version_in_place(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            releases = root / "releases"
            existing = releases / release_dirname(1, "1" * 64)
            (existing / "downloads/v0.1.3").mkdir(parents=True)
            (existing / "downloads/v0.1.3/old.zip").write_bytes(b"old-bytes")
            os.utime(existing, ns=(1_000_000_000, 1_000_000_000))

            _, counts = self._install_release(
                root,
                tag_index=2,
                version="v0.1.4",
                assets={"new.zip": b"new-bytes", "stale.zip": b"stale-bytes"},
                mtime_ns=2_000_000_000,
            )
            self.assertEqual(counts, (0, 2, 0))
            self.assertEqual((root / "downloads/v0.1.3/old.zip").read_bytes(), b"old-bytes")
            self.assertEqual((root / "downloads/v0.1.4/new.zip").read_bytes(), b"new-bytes")
            self.assertTrue((root / "downloads/v0.1.4/stale.zip").is_file())

            # Republishing v0.1.4 with a newer release replaces assets and drops stale ones.
            _, counts = self._install_release(
                root,
                tag_index=3,
                version="v0.1.4",
                assets={"new.zip": b"updated-bytes", "extra.zip": b"extra"},
                mtime_ns=3_000_000_000,
            )
            self.assertEqual(counts, (0, 2, 0))
            self.assertEqual((root / "downloads/v0.1.4/new.zip").read_bytes(), b"updated-bytes")
            self.assertEqual((root / "downloads/v0.1.4/extra.zip").read_bytes(), b"extra")
            self.assertFalse((root / "downloads/v0.1.4/stale.zip").exists())

    def test_handles_archive_without_downloads_and_rejects_hidden_download_entries(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            _, counts = self._install_release(
                root,
                tag_index=1,
                version=None,
                mtime_ns=1_000_000_000,
            )
            self.assertEqual(counts, (0, 0, 0))
            self.assertEqual(list((root / "downloads").iterdir()), [])

            bad_archive = root / "bad.tar.gz"
            make_archive(
                bad_archive,
                {
                    "web/index.html": b"<html></html>",
                    "downloads/.hidden/pkg.zip": b"bad",
                },
            )
            candidate = root / "bad-stage"
            candidate.mkdir()
            result = run_block(EXTRACT_BLOCK, str(bad_archive), str(candidate))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Unexpected hidden download entry", result.stderr)


if __name__ == "__main__":
    unittest.main()
