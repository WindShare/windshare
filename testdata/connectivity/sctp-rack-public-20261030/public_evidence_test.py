"""Publication must retain measurements and verify the bytes readers actually receive."""
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

from public_evidence import digest, hash_updates, write_archive


class PublicationTests(unittest.TestCase):
    def test_normalizes_nested_capture_metadata_and_repairs_hash_chain(self):
        event = {"ms": 21.75, "seq": 4, "remote": "10.1.2.3:3000",
                 "path": "D:\\private\\owner\\results\\run.jsonl",
                 "dependency": "/home/operator/fork", "loopback": "127.0.0.1:4000"}
        trace = (json.dumps(event) + "\n").encode()
        ledger = json.dumps({"trace_sha256": digest(trace), "recovery_ms": [21.75]}).encode()
        summary = json.dumps({"ledger_sha256": digest(ledger)}).encode()
        with tempfile.TemporaryDirectory() as folder:
            target = Path(folder) / "evidence.zip"
            published = write_archive(target, {
                "results/run.jsonl": trace, "results/ledger.json": ledger, "summary.json": summary,
            })
            sanitized = json.loads(published["results/run.jsonl"])
            self.assertEqual(sanitized, event | {"remote": "192.0.2.1:3000",
                                                "path": "results/run.jsonl", "dependency": "<local-path>"})
            self.assertEqual(json.loads(published["results/ledger.json"])["trace_sha256"],
                             digest(published["results/run.jsonl"]))
            self.assertEqual(json.loads(published["summary.json"])["ledger_sha256"],
                             digest(published["results/ledger.json"]))
            self.assertEqual(hash_updates(target)[digest(trace)], digest(published["results/run.jsonl"]))
            with zipfile.ZipFile(target) as archive:
                self.assertTrue(all(not entry.extra and not entry.comment for entry in archive.infolist()))

    def test_diagnostic_paths_preserve_the_error_context(self):
        diagnostic = (r"C:\Program Files\Go\src\net\net.go:4:2: could not import net "
                      r"(C:\\Users\\operator\\go\\pkg\\foo.go:1:2)")
        with tempfile.TemporaryDirectory() as folder:
            published = write_archive(Path(folder) / "evidence.zip", {"lint.txt": diagnostic.encode()})
            self.assertEqual(published["lint.txt"],
                             b"local/net.go:4:2: could not import net (local/foo.go:1:2)")

    def test_source_hashes_survive_repository_line_endings(self):
        source = b"package main\r\n"
        with tempfile.TemporaryDirectory() as folder:
            published = write_archive(Path(folder) / "evidence.zip", {
                "main.go": source, "summary.json": json.dumps({"sha256": digest(source)}).encode() + b"\r\n",
            })
            self.assertEqual(published["main.go"], b"package main\n")
            self.assertNotIn(b"\r\n", published["summary.json"])
            self.assertEqual(json.loads(published["summary.json"])["sha256"], digest(published["main.go"]))

    def test_followup_references_published_original(self):
        original = b"original"
        public = b"public"
        with tempfile.TemporaryDirectory() as folder:
            published = write_archive(Path(folder) / "followup.zip", {
                "summary.json": json.dumps({"original_evidence_sha256": digest(original)}).encode(),
            }, {digest(original): digest(public)})
            self.assertEqual(json.loads(published["summary.json"])["original_evidence_sha256"], digest(public))

    def test_rejects_credentials_and_unsafe_members_without_creating_archive(self):
        with tempfile.TemporaryDirectory() as folder:
            destination = Path(folder) / "evidence.zip"
            for members in ({"../outside.txt": b"capture"}, {"capture.txt": b"api_key=private-value"},
                            {"capture.txt": b"operator@example.com"}, {"capture.bin": b"\xff"}):
                with self.subTest(members=list(members)), self.assertRaises((ValueError, UnicodeDecodeError)):
                    write_archive(destination, members)
                self.assertFalse(destination.exists())

    def test_public_measurements_are_byte_stable(self):
        trace = b'{"ms":21.75,"seq":4,"remote":"127.0.0.1:3000","options":{"drops":2}}\n'
        with tempfile.TemporaryDirectory() as folder:
            published = write_archive(Path(folder) / "evidence.zip", {
                "results/run.jsonl": trace, "results/failed.summary.json": b"",
            })
            self.assertEqual(published["results/run.jsonl"], trace)
            self.assertEqual(published["results/failed.summary.json"], b"")


if __name__ == "__main__":
    unittest.main()
