#!/usr/bin/env python3
"""Catch update drift at the shared toolchain and workflow trust boundaries."""
import importlib.util
import hashlib
import io
import json
import pathlib
import re
import tomllib
import tempfile
import urllib.error
import unittest
import zipfile
from unittest.mock import patch

from ci_common import GitHub

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("policy", ROOT / "scripts/repository-policy.py")
policy_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy_module)


class ToolchainContractTests(unittest.TestCase):
    def test_go_module_mise_and_all_builders_use_one_patch_version(self):
        configuration = tomllib.loads((ROOT / "mise.toml").read_text())
        expected = configuration["tools"]["go"]
        self.assertRegex(expected, r"^[0-9]+\.[0-9]+\.[0-9]+$")
        module = re.search(r"^go (\S+)$", (ROOT / "go.mod").read_text(), re.MULTILINE).group(1)
        self.assertEqual(module, expected, "Go updates must synchronize mise, go.mod and builder images")
        for file in ("Containerfile", "scripts/FakeContainerfile"):
            with self.subTest(file=file):
                builder = re.search(r"^FROM docker.io/library/golang:([0-9.]+)-[^@]+@sha256:[0-9a-f]{64} AS build$",
                                    (ROOT / file).read_text(), re.MULTILINE)
                self.assertIsNotNone(builder, "builder must retain its reviewed digest")
                self.assertEqual(builder.group(1), expected)
        self.assertEqual(configuration["env"]["GOTOOLCHAIN"], "local")
        self.assertNotIn("@latest", (ROOT / "scripts/go-checks.sh").read_text())

    def test_all_actions_are_pinned_and_allowed_by_reviewed_repository_policy(self):
        policy = json.loads((ROOT / ".github/repository-policy.json").read_text())
        allowed_third_party = {pattern.split("@")[0] for pattern in policy["allowed_actions"]["patterns_allowed"]}
        mise_versions = []
        for file in sorted((ROOT / ".github/workflows").glob("*.yml")):
            content = file.read_text()
            refs = re.findall(r"uses:\s*(\S+)", content)
            self.assertTrue(refs)
            for ref in refs:
                owner, revision = ref.split("@")
                self.assertRegex(revision, r"^[0-9a-f]{40}$", "action must retain a full reviewed SHA")
                self.assertTrue(owner.startswith("actions/") or owner in allowed_third_party)
            mise_versions.extend(re.findall(r"\n\s+version: ([0-9.]+)", content))
            self.assertNotIn("pull_request_target:", content)
            self.assertNotIn("secrets.", content)
            self.assertNotIn("sudo podman", content)
            self.assertEqual(content.count("persist-credentials: false"), content.count("uses: actions/checkout@"))
        self.assertEqual(len(mise_versions), 6)
        self.assertEqual(len(set(mise_versions)), 1, "mise itself must update consistently across every job")



class APIReadTests(unittest.TestCase):
    def artifact_zip(self, name="receipt.json"):
        stream = io.BytesIO()
        with zipfile.ZipFile(stream, "w") as archive:
            archive.writestr(name, '{}')
        return stream.getvalue()

    def download(self, data, location="https://test.blob.core.windows.net/signed", expected_digest=None):
        api = GitHub("operator/service")
        api.token = "controlled-test-token"
        redirect = urllib.error.HTTPError("https://api.github.com/test", 302, "redirect", {"Location": location}, None)
        with tempfile.TemporaryDirectory() as directory:
            with patch("ci_common.urllib.request.build_opener") as opener, patch("ci_common.urllib.request.urlopen") as storage:
                opener.return_value.open.side_effect = redirect
                storage.return_value = io.BytesIO(data)
                api.download(103, directory, ["receipt.json"], expected_digest or "sha256:" + hashlib.sha256(data).hexdigest())
                self.assertEqual((pathlib.Path(directory) / "receipt.json").read_text(), '{}')
                # Storage receives only the signed URL, never the GitHub request or its token.
                self.assertEqual(storage.call_args.args[0], location)

    def test_artifact_download_verifies_zip_and_does_not_forward_github_credentials(self):
        self.download(self.artifact_zip())

    def test_artifact_download_rejects_unknown_hosts_path_traversal_and_wrong_zip_digest(self):
        for data, host, checksum in ((self.artifact_zip(), "https://example.com/steal", None),
                                     (self.artifact_zip("../receipt.json"), "https://test.blob.core.windows.net/signed", None),
                                     (self.artifact_zip(), "https://test.blob.core.windows.net/signed", "sha256:" + "f" * 64)):
            with self.assertRaises(ValueError):
                self.download(data, host, checksum)

    def test_pagination_reads_all_pages_and_fails_instead_of_truncating(self):
        api = GitHub("operator/service")
        with patch.object(api, "get", side_effect=[{"items": [1] * 100}, {"items": [2]}]):
            self.assertEqual(len(api.pages("path", "items")), 101)
        with patch.object(api, "get", return_value={"items": [1] * 100}):
            with self.assertRaises(ValueError):
                api.pages("path", "items")

    def test_read_back_detects_extra_bypass_and_independent_approval_requirements(self):
        expected = {"bypass_actors": [], "rules": [{"type": "pull_request", "parameters": {
            "required_approving_review_count": 0, "required_reviewers": [],
            "require_extra_approval_for_unattributed_changes": False}}]}
        actual = {"id": 99, **expected}
        self.assertTrue(policy_module.subset(actual, expected))
        self.assertFalse(policy_module.subset({**actual, "bypass_actors": [{"actor_id": 1}]}, expected))
        self.assertFalse(policy_module.subset({**actual, "rules": [{"type": "pull_request", "parameters": {
            "required_approving_review_count": 1}}]}, expected))
        extra_approval = {**actual, "rules": [{"type": "pull_request", "parameters": {
            **expected["rules"][0]["parameters"], "require_extra_approval_for_unattributed_changes": True}}]}
        self.assertFalse(policy_module.subset(extra_approval, expected))


if __name__ == "__main__":
    unittest.main()
