#!/usr/bin/env python3
import base64
import copy
import hashlib
import json
import pathlib
import stat
import shutil
import tempfile
import unittest
from unittest.mock import patch

import ci_fixtures as fixtures
import ci_common
import release


class FakeGitHub:
    repository = "operator/service"

    def __init__(self, root, receipt):
        self.root = root
        self.receipt = receipt
        self.repository_info = copy.deepcopy(fixtures.REPOSITORY)
        self.rules = [{"type": "pull_request", "parameters": {"allowed_merge_methods": ["squash"]}}]
        self.pr = copy.deepcopy(fixtures.PR)
        self.run = copy.deepcopy(fixtures.RUN)
        self.jobs = copy.deepcopy(fixtures.JOBS)
        self.artifacts = fixtures.artifacts()
        self.commit = {"sha": "e" * 40, "parents": [{"sha": "f" * 40}], "commit": {"tree": {"sha": "d" * 40}}}
        self.tested = {"tree": {"sha": "d" * 40}, "parents": [{"sha": "b" * 40}, {"sha": "a" * 40}]}
        self.check = {"id": 80, "name": "CI", "app": {"id": 15368}, "conclusion": "success"}
        self.status = "ahead"
        self.version = "1.2.3"

    def get(self, path):
        if path == "":
            return self.repository_info
        if path == "rules/branches/main":
            return self.rules
        if path == "commits/v1.2.3":
            return self.commit
        if path == "compare/" + "e" * 40 + "...main":
            return {"status": self.status}
        if path.startswith("contents/VERSION?"):
            return {"encoding": "base64", "content": base64.b64encode(self.version.encode()).decode()}
        if path == "pulls/12":
            return self.pr
        if path == "actions/workflows/ci.yml":
            return {"id": 456}
        if path == "actions/runs/789":
            return self.run
        if path == "check-runs/80":
            return self.check
        if path.startswith("git/commits/"):
            return self.tested
        raise ValueError("Unexpected fake API read: " + path)

    def pages(self, path, key=None):
        if path.endswith("/pulls"):
            return [self.pr]
        if path.startswith("actions/workflows/ci.yml/runs?"):
            return [self.run]
        if path == "actions/runs/789/attempts/2/jobs":
            return self.jobs
        if path == "actions/runs/789/artifacts":
            return self.artifacts
        raise ValueError("Unexpected fake API collection: " + path)

    def download(self, artifact_id, directory, expected_files, expected_digest):
        directory.mkdir(parents=True, exist_ok=True)
        if artifact_id == 103:
            (directory / "receipt.json").write_text(json.dumps(self.receipt))
        else:
            architecture = {101: "amd64", 102: "arm64"}[artifact_id]
            for name in expected_files:
                shutil.copyfile(self.root / architecture / name, directory / name)


class ReleaseVerificationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        fixtures.platforms(self.root)
        receipt = release.oci.make_receipt(self.root, fixtures.SOURCE, "1.2.3", {"go": "success", "podman": "success"},
                                           fixtures.artifacts(), fixtures.TIME)
        self.api = FakeGitHub(self.root, receipt)
        self.clock = patch.object(ci_common, "now", return_value=fixtures.TIME)
        self.clock.start()
        self.addCleanup(self.clock.stop)
        self.clock2 = patch.object(release, "now", return_value=fixtures.TIME)
        self.clock2.start()
        self.addCleanup(self.clock2.stop)

    def prepare(self):
        return release.prepare(self.api, fixtures.EVENT, self.root / "download")

    def test_success_resolves_squash_merge_with_different_commit_and_identical_tree(self):
        plan = self.prepare()
        self.assertNotEqual(plan["commit"], plan["source"]["checkout"])
        self.assertEqual(plan["tree"], plan["source"]["tree"])
        self.assertEqual(plan["platforms"]["amd64"]["artifact"]["id"], 101)

    def test_read_only_repository_view_uses_effective_squash_protection(self):
        for key in ("allow_squash_merge", "allow_merge_commit", "allow_rebase_merge"):
            self.api.repository_info.pop(key)
        self.prepare()
        for rules in ([], [{"type": "pull_request", "parameters": {"allowed_merge_methods": ["squash", "rebase"]}}]):
            self.api.rules = rules
            with self.assertRaises(ValueError):
                self.prepare()

    def test_direct_tag_wrong_version_and_unreachable_main_fail(self):
        for target, key, value in ((self.api.pr, "merge_commit_sha", "f" * 40),
                                   (self.api.pr, "merged_at", None),
                                   (self.api.pr["base"], "ref", "other"),
                                   (self.api.commit, "parents", [{"sha": "a" * 40}, {"sha": "b" * 40}])):
            original = copy.deepcopy(target[key])
            target[key] = value
            with self.assertRaises(ValueError):
                self.prepare()
            target[key] = original
        self.api.version = "1.2.4"
        with self.assertRaises(ValueError):
            self.prepare()
        self.api.version = "1.2.3"
        self.api.status = "diverged"
        with self.assertRaises(ValueError):
            self.prepare()

    def test_ambiguous_pr_association_rejected(self):
        with self.assertRaises(ValueError):
            release.select_pr([fixtures.PR, fixtures.PR], "e" * 40, 123)

    def test_failed_newer_run_does_not_reuse_old_success(self):
        newer = copy.deepcopy(fixtures.RUN)
        newer.update(id=790, conclusion="failure")
        with self.assertRaises(ValueError):
            release.select_run([fixtures.RUN, newer], fixtures.PR, 123, 456)

    def test_receipt_repo_workflow_attempt_tree_revision_mismatches_rejected(self):
        source = self.api.receipt["source"]
        for key, value in (("repository_id", 999), ("workflow_id", 999), ("run_attempt", 1), ("pr_number", 999),
                           ("head", "f" * 40), ("tree", "f" * 40)):
            original = source[key]
            source[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.prepare()
            source[key] = original
        self.api.tested["parents"][0]["sha"] = "f" * 40
        with self.assertRaises(ValueError):
            self.prepare()

    def test_failed_skipped_cancelled_and_missing_current_attempt_jobs_rejected(self):
        for result in ("failure", "skipped", "cancelled", None):
            self.api.jobs[1]["conclusion"] = result
            with self.subTest(result=result), self.assertRaises(ValueError):
                self.prepare()
        self.api.jobs.pop(1)
        with self.assertRaises(ValueError):
            self.prepare()

    def test_wrong_check_publisher_expired_artifact_or_tampered_archive_rejected(self):
        self.api.check["app"]["id"] = 1
        with self.assertRaises(ValueError):
            self.prepare()
        self.api.check["app"]["id"] = 15368
        self.api.artifacts[0]["expired"] = True
        with self.assertRaises(ValueError):
            self.prepare()
        self.api.artifacts[0]["expired"] = False
        fixtures.write_archive(self.root / "arm64/image.oci.tar", corrupt=True)
        with self.assertRaises(ValueError):
            self.prepare()

    def test_release_tag_and_prerelease_flag_contract(self):
        for tag, flag in (("v1.2.3", True), ("v1.2.3-rc.1", False), ("v1.2.3+meta", False), ("1.2.3", False)):
            event = copy.deepcopy(fixtures.EVENT)
            event["release"].update(tag_name=tag, prerelease=flag)
            with self.assertRaises(ValueError):
                release.validate_release(event)
        event = copy.deepcopy(fixtures.EVENT)
        event["release"].update(tag_name="v1.2.3-rc.1", prerelease=True)
        self.assertEqual(release.validate_release(event), ("v1.2.3-rc.1", "1.2.3-rc.1"))
        event["release"]["draft"] = True
        with self.assertRaises(ValueError):
            release.validate_release(event)


class FakeRegistry:
    def __init__(self):
        self.contents = {}
        self.writes = []
        self.fail_tag = None

    def manifest(self, reference):
        return self.contents.get(reference)

    def put(self, reference, data):
        if reference == self.fail_tag:
            raise RuntimeError("controlled registry failure")
        identity = "sha256:" + hashlib.sha256(data).hexdigest()
        self.contents[reference] = identity, json.loads(data), data
        self.writes.append(reference)


class PromotionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.plan = {"version": "1.2.3", "tag": "v1.2.3", "prerelease": False, "repository": "operator/service",
                     "package": "operator/service", "source": copy.deepcopy(fixtures.SOURCE), "platforms": fixtures.platforms(self.root)}
        self.registry = FakeRegistry()
        for architecture, metadata in self.plan["platforms"].items():
            image = metadata["image"]
            self.registry.contents["v1.2.3-" + architecture] = (
                image["manifest"], {"config": {"digest": image["config"]},
                                    "layers": [{"digest": layer} for layer in image["layers"]]}, b"fixture")

    def latest(self, version):
        previous = copy.deepcopy(self.plan)
        previous.update(version=version, tag="v" + version)
        data = release.index_bytes(previous)
        self.registry.put("v" + version, data)
        self.registry.put("latest", data)
        self.registry.writes.clear()

    def test_exact_platform_index_publication_and_idempotent_retry(self):
        expected = "sha256:" + hashlib.sha256(release.index_bytes(self.plan)).hexdigest()
        self.assertEqual(release.publish_index(self.plan, self.registry), (expected, True))
        self.assertEqual(self.registry.writes, ["v1.2.3", "latest"])
        self.registry.writes.clear()
        self.assertEqual(release.publish_index(self.plan, self.registry), (expected, False))
        self.assertEqual(self.registry.writes, [])
        actual = self.registry.manifest("v1.2.3")[1]
        self.assertEqual([m["platform"]["architecture"] for m in actual["manifests"]], ["amd64", "arm64"])

    def test_collision_and_incomplete_components_fail_before_index_or_latest_write(self):
        self.registry.contents["v1.2.3"] = "sha256:" + "f" * 64, {}, b"other"
        with self.assertRaises(ValueError):
            release.publish_index(self.plan, self.registry)
        self.assertEqual(self.registry.writes, [])
        del self.registry.contents["v1.2.3"]
        del self.registry.contents["v1.2.3-arm64"]
        with self.assertRaises(ValueError):
            release.publish_index(self.plan, self.registry)
        self.assertEqual(self.registry.writes, [])

    def test_prerelease_and_older_stable_never_move_latest(self):
        self.latest("2.0.0")
        release.publish_index(self.plan, self.registry)
        self.assertEqual(self.registry.writes, ["v1.2.3"])
        self.registry.writes.clear()
        self.plan.update(prerelease=True)
        release.publish_index(self.plan, self.registry)
        self.assertEqual(self.registry.writes, [])

    def test_newer_stable_advances_latest(self):
        self.latest("1.1.9")
        release.publish_index(self.plan, self.registry)
        self.assertEqual(self.registry.writes, ["v1.2.3", "latest"])

    def test_unrecognized_latest_requires_operator_resolution(self):
        self.registry.contents["latest"] = "sha256:" + "f" * 64, {}, b"unknown"
        with self.assertRaises(ValueError):
            release.publish_index(self.plan, self.registry)
        self.assertEqual(self.registry.writes, [])

    def test_partial_publish_retry_never_moves_latest_until_version_complete(self):
        self.registry.fail_tag = "v1.2.3"
        with self.assertRaises(RuntimeError):
            release.publish_index(self.plan, self.registry)
        self.assertNotIn("latest", self.registry.contents)
        self.registry.fail_tag = None
        release.publish_index(self.plan, self.registry)
        self.assertEqual(self.registry.writes, ["v1.2.3", "latest"])

    def test_component_collision_does_not_load_or_push(self):
        image = self.plan["platforms"]["amd64"]["image"]
        self.registry.contents["v1.2.3-amd64"] = "sha256:" + "f" * 64, {}, b"other"
        with patch.object(release, "rootless", return_value={"host": {"arch": "amd64"}}), patch.object(release, "command") as command:
            with self.assertRaises(ValueError):
                release.publish_component(self.plan, "amd64", self.root, self.registry)
            command.assert_not_called()
        self.registry.contents["v1.2.3-amd64"] = image["manifest"], {}, b"same"
        with patch.object(release, "rootless", return_value={"host": {"arch": "amd64"}}), patch.object(release, "command") as command:
            self.assertEqual(release.publish_component(self.plan, "amd64", self.root, self.registry), image["manifest"])
            command.assert_not_called()

    def test_private_auth_file_is_valid_before_login_and_token_is_stdin_only(self):
        def login(*args, input=None):
            path = pathlib.Path(args[args.index("--authfile") + 1])
            self.assertEqual(json.loads(path.read_text()), {"auths": {}})
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(input, "controlled-test-token")
            self.assertNotIn(input, args)
            return "Login Succeeded"

        with patch.dict("os.environ", {"GITHUB_ACTOR": "operator", "GH_TOKEN": "controlled-test-token"}), \
                patch.object(release, "command", side_effect=login):
            with tempfile.TemporaryDirectory() as directory:
                self.assertEqual(release.auth_file(directory), pathlib.Path(directory) / "auth.json")


if __name__ == "__main__":
    unittest.main()
