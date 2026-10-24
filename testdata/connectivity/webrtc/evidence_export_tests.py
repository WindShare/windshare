"""Publication-boundary regressions; all examples are synthetic."""
import unittest
from evidence_export import PublicEvidence

class PublicationTests(unittest.TestCase):
    def test_measurements_survive_nested_private_fields(self):
        raw = {"MBps": 31.5, "sha256": "a" * 64, "candidate": {
            "address": "192.0.2.5", "usernameFragment": "synthetic-ice-secret",
            "id": "candidate-original", "candidateType": "host", "protocol": "udp"},
            "localCandidateId": "candidate-original"}
        result = PublicEvidence().project(raw)
        self.assertEqual(result["MBps"], raw["MBps"])
        self.assertEqual(result["sha256"], raw["sha256"])
        self.assertEqual(result["candidate"]["id"], result["localCandidateId"])
        self.assertEqual(result["candidate"]["candidateType"], "host")
        self.assertNotIn("synthetic-ice-secret", str(result))
        self.assertNotIn("192.0.2.5", str(result))

    def test_trial_identity_correlates_without_publishing_original_values(self):
        raw = {"fault": {"pathId": "synthetic-trial-a", "laneId": 3, "laneEpoch": 2},
            "decisions": [{"trial_path": "synthetic-trial-a", "completedBytes": 8_388_608},
                {"trial_path": "synthetic-trial-b", "trial_lane_id": 3, "trial_lane_epoch": 3}]}
        result = PublicEvidence().project(raw)
        self.assertEqual(result["fault"]["pathId"], result["decisions"][0]["trial_path"])
        self.assertNotEqual(result["fault"]["pathId"], result["decisions"][1]["trial_path"])
        self.assertEqual(result["fault"]["laneEpoch"], 2)
        self.assertEqual(result["decisions"][1]["trial_lane_epoch"], 3)
        self.assertEqual(result["decisions"][0]["completedBytes"], 8_388_608)
        self.assertNotIn("synthetic-trial", str(result))

    def test_socket_ports_do_not_escape_through_native_or_browser_field_names(self):
        raw = {"local_port": 40001, "remotePort": 40002, "candidate": {"port": 40003},
            "protocol": "udp", "local_type": "host", "remote_type": "srflx", "MBps": 31.5}
        result = PublicEvidence().project(raw)
        for value in ["40001", "40002", "40003"]:
            self.assertNotIn(value, str(result))
        for field in ["protocol", "local_type", "remote_type", "MBps"]:
            self.assertEqual(result[field], raw[field])

    def test_logs_paths_addresses_and_versions(self):
        raw = {"error": "failed at C:/Users/example/lab/run.js\nconnect 2001:db8::1",
            "cleanup": {"profile": "/home/example/.cache/browser"},
            "browser": "155.0.8059.84", "stderr": "private connection command",
            "url": "https://example.invalid/download#synthetic-capability"}
        result = PublicEvidence().project(raw)
        self.assertEqual(result["browser"], raw["browser"])
        for private in ["C:/Users", "/home/example", "2001:db8::1", "synthetic-capability", "private connection command"]:
            self.assertNotIn(private, str(result))

    def test_endpoint_names_are_removed_from_artifact_references(self):
        result = PublicEvidence().project({"endpoint": "private-lab-machine",
            "raw": ".tmp/native-private-lab-machine-forward/report.json"})
        self.assertNotIn("private-lab-machine", str(result))

    def test_credentials_in_collections_and_error_text(self):
        result = PublicEvidence().project({"credentials": {"shared_secret": "synthetic-value"},
            "error": "request token=synthetic-token password=synthetic-password",
            "status": "passed"})
        self.assertNotIn("synthetic", str(result))
        self.assertEqual(result["status"], "passed")

if __name__ == "__main__":
    unittest.main()
