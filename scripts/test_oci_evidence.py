#!/usr/bin/env python3
import copy
import datetime
import importlib.util
import json
import pathlib
import tempfile
import unittest

import ci_fixtures as fixtures
from ci_common import RUNTIME_CHECKS, version_parts

spec = importlib.util.spec_from_file_location("oci", pathlib.Path(__file__).with_name("oci-evidence.py"))
oci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(oci)


class ArchiveTests(unittest.TestCase):
    def test_manifest_config_platform_and_layer_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "image.tar"
            image = fixtures.write_archive(path)
            self.assertEqual(oci.archive_identity(path), image)
            for corrupt, duplicate in ((True, False), (False, True)):
                fixtures.write_archive(path, corrupt=corrupt, duplicate=duplicate)
                with self.assertRaises(ValueError):
                    oci.archive_identity(path)

    def test_unsupported_platform_root_identity_and_invalid_version_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "image.tar"
            for architecture, user, version in [("386", "65532:65532", "1.2.3"), ("amd64", "0:0", "1.2.3"),
                                                ("arm64", "65532:65532", "1.2.3+metadata")]:
                fixtures.write_archive(path, architecture=architecture, user=user, version=version)
                with self.assertRaises(ValueError):
                    oci.archive_identity(path)

    def test_version_contract(self):
        for invalid in ("v1.2.3", "01.2.3", "1.2", "1.2.3-01", "1.2.3+meta", "1.2.3\n", "1.2.3-", "$(env)"):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                version_parts(invalid)
        self.assertEqual(version_parts("1.2.3-rc.1"), ((1, 2, 3), "rc.1"))


class ReceiptTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.metadata = fixtures.platforms(self.root)
        self.artifacts = fixtures.artifacts()
        self.outcomes = {"go": "success", "podman": "success"}

    def receipt(self):
        return oci.make_receipt(self.root, fixtures.SOURCE, "1.2.3", self.outcomes, self.artifacts, fixtures.TIME)

    def test_success_binds_both_native_images_to_server_artifact_ids(self):
        receipt = self.receipt()
        self.assertEqual(receipt["platforms"]["amd64"]["artifact"]["id"], 101)
        self.assertEqual(receipt["platforms"]["arm64"]["artifact"]["id"], 102)
        self.assertEqual(receipt["source"], fixtures.SOURCE)

    def test_failed_skipped_cancelled_missing_prerequisites_fail_gate(self):
        for job in self.outcomes:
            for result in ("failure", "skipped", "cancelled", ""):
                with self.subTest(job=job, result=result):
                    self.outcomes = {"go": "success", "podman": "success", job: result}
                    with self.assertRaises(ValueError):
                        self.receipt()

    def test_missing_expired_and_mixed_attempt_artifacts_fail(self):
        cases = []
        cases.append(self.artifacts[1:])
        for key, value in (("expired", True), ("name", "image-amd64-789-1"), ("id", 0),
                           ("created_at", (fixtures.TIME - datetime.timedelta(days=6)).isoformat())):
            modified = copy.deepcopy(self.artifacts)
            modified[0][key] = value
            cases.append(modified)
        modified = copy.deepcopy(self.artifacts)
        modified[0]["workflow_run"]["head_sha"] = "e" * 40
        cases.append(modified)
        for artifacts in cases:
            self.artifacts = artifacts
            with self.assertRaises(ValueError):
                self.receipt()

    def test_metadata_substitution_and_missing_named_runtime_checks_fail(self):
        original = copy.deepcopy(self.metadata["amd64"])
        for change in (lambda m: m["source"].update(run_attempt=1),
                       lambda m: m["source"].update(repository_id=999),
                       lambda m: m["runtime"].update(rootless=False),
                       lambda m: m["runtime"].update(uid=0),
                       lambda m: m["runtime"].update(image_config="sha256:" + "f" * 64),
                       lambda m: m["runtime"].update(shutdown_seconds=25),
                       lambda m: m["runtime"].update(checks=["invented"] * len(RUNTIME_CHECKS))):
            metadata = copy.deepcopy(original)
            change(metadata)
            (self.root / "amd64/metadata.json").write_text(json.dumps(metadata))
            (self.root / "amd64/runtime.json").write_text(json.dumps(metadata["runtime"]))
            with self.assertRaises(ValueError):
                self.receipt()

    def test_tampering_archive_after_runtime_cannot_produce_receipt(self):
        fixtures.write_archive(self.root / "amd64/image.oci.tar", "amd64", corrupt=True)
        with self.assertRaises(ValueError):
            self.receipt()


if __name__ == "__main__":
    unittest.main()
