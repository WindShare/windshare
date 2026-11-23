import argparse
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import tomllib
import unittest
from unittest.mock import patch


def load_module():
    path = Path(__file__).resolve().with_name("network-authority.py")
    spec = importlib.util.spec_from_file_location("network_authority_deploy", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


DEPLOY = load_module()
VALID_TOKEN = "a" * 32
VALID_PEPPER = "v1:" + "W" * 42 + "Q"
DIRECTORY_ENV = {
    "DIRECTORY_KV_ID": "1" * 32,
    "DIRECTORY_CERTIFICATE": '{"generation":1}',
    "DIRECTORY_SIGNING_SEED": "A" * 43,
    "PASSWORD_PEPPER": VALID_PEPPER,
}


class VerifyReleaseAuthorityTests(unittest.TestCase):
    def test_accepts_matching_origin_and_root(self):
        release = {
            "authority": {
                "url": "https://api.ourworld.top/",
                "root": "root-public-key",
            }
        }
        DEPLOY.verify_release_authority(release, "https://api.ourworld.top", "root-public-key")

    def test_allows_unprovisioned_authority(self):
        DEPLOY.verify_release_authority({}, "https://api.ourworld.top", "root-public-key")
        DEPLOY.verify_release_authority({"authority": None}, "https://api.ourworld.top", "root-public-key")

    def test_rejects_root_mismatch(self):
        release = {
            "authority": {
                "url": "https://api.ourworld.top",
                "root": "prod-root",
            }
        }
        with self.assertRaisesRegex(ValueError, "staging-root.*prod-root"):
            DEPLOY.verify_release_authority(release, "https://api.ourworld.top", "staging-root")

    def test_rejects_origin_mismatch(self):
        release = {
            "authority": {
                "url": "https://api.ourworld.top",
                "root": "prod-root",
            }
        }
        with self.assertRaisesRegex(ValueError, "https://staging.example.com.*https://api.ourworld.top"):
            DEPLOY.verify_release_authority(release, "https://staging.example.com", "prod-root")

    def test_rejects_malformed_authority_section(self):
        for bad in (
            {"authority": "https://api.ourworld.top"},
            {"authority": {"url": "https://api.ourworld.top"}},
            {"authority": {"root": "prod-root"}},
            {"authority": {"url": "https://api.ourworld.top", "root": ""}},
            {"authority": {"url": "http://api.ourworld.top", "root": "prod-root"}},
        ):
            with self.subTest(bad=bad):
                with self.assertRaises(ValueError):
                    DEPLOY.verify_release_authority(bad, "https://api.ourworld.top", "prod-root")


class DeployReleaseConsistencyTests(unittest.TestCase):
    def _stage_deploy(
        self,
        root_dir,
        *,
        api_host="api.ourworld.top",
        origin_host=None,
        zone=None,
        trust_root="prod-root",
        release_data=None,
    ):
        crate = root_dir / "crate"
        crate.mkdir()
        (crate / "wrangler.toml").write_text('name = "windshare-network-authority"\n', encoding="utf-8")
        trust_path = root_dir / "trust.json"
        trust_path.write_text(json.dumps({"root": trust_root, "certificate": "cert-1"}), encoding="utf-8")
        seed_path = root_dir / "online.seed"
        seed_path.write_text("seed-1", encoding="utf-8")
        token_path = root_dir / "init.token"
        token_path.write_text(VALID_TOKEN, encoding="utf-8")
        release_path = None
        if release_data is not None:
            release_path = root_dir / "release.json"
            release_path.write_text(json.dumps(release_data), encoding="utf-8")
        args = argparse.Namespace(
            api_host=api_host,
            worker_name="windshare-network-authority",
            origin_host=origin_host,
            zone=zone,
            trusted_proxy_ips="",
            allowed_origins="https://ws.ourworld.top",
            trust=trust_path,
            online_seed=seed_path,
            initialization_token=token_path,
            password_pepper=None,
            prepare_initialization=None,
            prepare_pepper=None,
            network_release=release_path,
        )
        return crate, args

    def test_mismatched_release_fails_before_subprocesses(self):
        with tempfile.TemporaryDirectory() as tmp:
            crate, args = self._stage_deploy(
                Path(tmp),
                trust_root="staging-root",
                release_data={
                    "authority": {"url": "https://api.ourworld.top", "root": "prod-root"},
                },
            )
            with (
                patch.object(DEPLOY, "CRATE", crate),
                patch.dict(os.environ, {}, clear=True),
                patch.object(DEPLOY.subprocess, "check_output") as check_output,
                patch.object(DEPLOY.subprocess, "run") as run,
            ):
                with self.assertRaisesRegex(ValueError, "staging-root.*prod-root"):
                    DEPLOY.deploy(args)
                check_output.assert_not_called()
                run.assert_not_called()

    def test_matching_release_verifies_and_publishes(self):
        with tempfile.TemporaryDirectory() as tmp:
            crate, args = self._stage_deploy(
                Path(tmp),
                trust_root="prod-root",
                release_data={
                    "authority": {"url": "https://api.ourworld.top", "root": "prod-root"},
                },
            )
            args.origin_host = 'origin.ourworld.top'
            args.zone = 'ourworld.top'
            generated_configs = []

            def fake_run(cmd, **kwargs):
                if "--config" in cmd:
                    config_path = Path(cmd[cmd.index("--config") + 1])
                    generated_configs.append(tomllib.loads(config_path.read_text(encoding="utf-8")))

            with (
                patch.object(DEPLOY, "CRATE", crate),
                patch.dict(os.environ, DIRECTORY_ENV, clear=True),
                patch.object(DEPLOY.subprocess, "run", side_effect=fake_run) as run,
                patch("builtins.print"),
            ):
                DEPLOY.deploy(args)
            commands = [call.args[0] for call in run.call_args_list]
            self.assertEqual(
                [cmd[cmd.index("--") + 1] for cmd in commands[:-1]],
                ["verify", "verify-publisher"],
            )
            self.assertIn("deploy", commands[-1])
            [config] = generated_configs
            self.assertEqual(
                config["routes"],
                [
                    {"pattern": "origin.ourworld.top", "custom_domain": True},
                    {"pattern": "api.ourworld.top/*", "zone_name": "ourworld.top"},
                ],
            )
            self.assertEqual(config["vars"]["AUTHORITY_ROOT"], "prod-root")
            self.assertEqual(config["vars"]["API_ORIGIN"], "https://api.ourworld.top")
            self.assertEqual(config["vars"]["AUTHORITY_ORIGIN"], "https://origin.ourworld.top")
            self.assertEqual(config["kv_namespaces"], [{"binding": "DIRECTORY", "id": DIRECTORY_ENV["DIRECTORY_KV_ID"]}])
            self.assertNotIn("DIRECTORY_PUBLICATION", config["vars"])
            self.assertNotIn("DIRECTORY_ORIGIN", config["vars"])

    def test_same_origin_and_host_omits_authority_origin_and_saas_route(self):
        for origin_host in (None, "", "api.ourworld.top"):
            with self.subTest(origin_host=origin_host):
                with tempfile.TemporaryDirectory() as tmp:
                    crate, args = self._stage_deploy(
                        Path(tmp),
                        api_host="api.ourworld.top",
                        origin_host=origin_host,
                    )
                    generated_configs = []

                    def fake_run(cmd, **kwargs):
                        if "--config" in cmd:
                            config_path = Path(cmd[cmd.index("--config") + 1])
                            generated_configs.append(tomllib.loads(config_path.read_text(encoding="utf-8")))

                    with (
                        patch.object(DEPLOY, "CRATE", crate),
                        patch.dict(os.environ, DIRECTORY_ENV, clear=True),
                        patch.object(DEPLOY.subprocess, "run", side_effect=fake_run),
                        patch("builtins.print"),
                    ):
                        DEPLOY.deploy(args)
                    [config] = generated_configs
                    self.assertEqual(
                        config["routes"],
                        [{"pattern": "api.ourworld.top", "custom_domain": True}],
                    )
                    self.assertEqual(config["vars"]["API_ORIGIN"], "https://api.ourworld.top")
                    self.assertNotIn("AUTHORITY_ORIGIN", config["vars"])

    def test_rejects_non_bare_api_and_origin_hosts(self):
        invalid_hosts = (
            "api.ourworld.top/",
            "api.ourworld.top?",
            "api.ourworld.top#",
            "api.ourworld.top\n",
            "api.ourworld.top\t",
            "https://api.ourworld.top",
            "API.ourworld.top",
        )
        for bad in invalid_hosts:
            with self.subTest(flag="--api-host", value=bad):
                with tempfile.TemporaryDirectory() as tmp:
                    crate, args = self._stage_deploy(
                        Path(tmp),
                        api_host=bad,
                        origin_host="api.ourworld.top",
                        zone="ourworld.top",
                    )
                    with (
                        patch.object(DEPLOY, "CRATE", crate),
                        patch.dict(os.environ, DIRECTORY_ENV, clear=True),
                        patch.object(DEPLOY.subprocess, "run") as run,
                    ):
                        with self.assertRaisesRegex(ValueError, "--api-host"):
                            DEPLOY.deploy(args)
                        run.assert_not_called()
            with self.subTest(flag="--prepare-initialization", value=bad):
                with tempfile.TemporaryDirectory() as tmp:
                    token_file = Path(tmp) / "prepared.token"
                    crate, args = self._stage_deploy(Path(tmp), api_host=bad)
                    args.prepare_initialization = token_file
                    with patch.object(DEPLOY, "CRATE", crate), patch.dict(os.environ, {}, clear=True):
                        with self.assertRaisesRegex(ValueError, "--api-host"):
                            DEPLOY.deploy(args)
                        with self.assertRaisesRegex(ValueError, "--api-host"):
                            DEPLOY.prepare_token(token_file, bad)
                    self.assertFalse(token_file.exists())
            with self.subTest(flag="--origin-host", value=bad):
                with tempfile.TemporaryDirectory() as tmp:
                    crate, args = self._stage_deploy(
                        Path(tmp),
                        api_host="api.ourworld.top",
                        origin_host=bad,
                        zone="ourworld.top",
                    )
                    with (
                        patch.object(DEPLOY, "CRATE", crate),
                        patch.dict(os.environ, DIRECTORY_ENV, clear=True),
                        patch.object(DEPLOY.subprocess, "run") as run,
                    ):
                        with self.assertRaisesRegex(ValueError, "--origin-host"):
                            DEPLOY.deploy(args)
                        run.assert_not_called()

    def test_deploy_without_network_release_skips_manifest_checks(self):
        with tempfile.TemporaryDirectory() as tmp:
            crate, args = self._stage_deploy(Path(tmp), api_host="staging.example.com", trust_root="staging-root")
            with (
                patch.object(DEPLOY, "CRATE", crate),
                patch.dict(os.environ, DIRECTORY_ENV, clear=True),
                patch.object(DEPLOY, "verify_release_authority") as verify_release,
                patch.object(DEPLOY.subprocess, "run") as run,
                patch("builtins.print"),
            ):
                DEPLOY.deploy(args)
                verify_release.assert_not_called()
                self.assertIn("deploy", run.call_args.args[0])


class PasswordPepperTests(unittest.TestCase):
    def test_validates_single_and_rotated_pepper_rings(self):
        v1 = "v1:" + "W" * 42 + "Q"
        v2 = "v2:" + "X" * 42 + "Q"
        self.assertEqual(DEPLOY.validate_password_pepper(f" {v2} , {v1} "), f"{v2},{v1}")
        for bad in (
            "",
            "missing-colon",
            f"BAD:{'W' * 42}Q",
            f"{v1},{v1}",
            "v1:short",
            f"v1:{'A' * 43}",
            f"v1:{'W' * 42}B",
        ):
            with self.subTest(bad=bad):
                with self.assertRaises(ValueError):
                    DEPLOY.validate_password_pepper(bad)

    def test_prepare_pepper_persists_once_and_refuses_ci(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "pepper.secret"
            with patch.dict(os.environ, {}, clear=True), patch("builtins.print"):
                DEPLOY.prepare_pepper(path)
                first = path.read_text(encoding="utf-8")
                self.assertTrue(first.startswith("v1:"))
                self.assertEqual(DEPLOY.validate_password_pepper(first), first)
                DEPLOY.prepare_pepper(path)
                self.assertEqual(path.read_text(encoding="utf-8"), first)
            with patch.dict(os.environ, {"CI": "true"}, clear=True):
                with self.assertRaisesRegex(ValueError, "operator machine"):
                    DEPLOY.prepare_pepper(path)


if __name__ == "__main__":
    unittest.main()
