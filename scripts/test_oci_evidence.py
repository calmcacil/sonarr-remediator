#!/usr/bin/env python3
import hashlib
import importlib.util
import io
import json
import pathlib
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("oci", pathlib.Path(__file__).with_name("oci-evidence.py"))
oci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(oci)


class ArchiveTests(unittest.TestCase):
    def archive(self, path, corrupt=False, architecture="arm64", user="65532:65532"):
        config = json.dumps({"os": "linux", "architecture": architecture, "config": {
            "User": user, "Labels": {"org.opencontainers.image.version": "0.1.0",
                                     "org.opencontainers.image.revision": "revision"}}}).encode()
        config_sha = hashlib.sha256(config).hexdigest()
        layer = b"controlled layer data"
        layer_sha = hashlib.sha256(layer).hexdigest()
        manifest = json.dumps({"mediaType": "application/vnd.oci.image.manifest.v1+json",
                               "config": {"digest": "sha256:" + config_sha},
                               "layers": [{"digest": "sha256:" + layer_sha}]}).encode()
        manifest_sha = hashlib.sha256(manifest).hexdigest()
        members = {"index.json": json.dumps({"manifests": [{"digest": "sha256:" + manifest_sha}]}).encode(),
                   "blobs/sha256/" + config_sha: config,
                   "blobs/sha256/" + layer_sha: b"tampered" if corrupt else layer,
                   "blobs/sha256/" + manifest_sha: manifest}
        with tarfile.open(path, "w") as archive:
            for name, data in members.items():
                member = tarfile.TarInfo(name)
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
        return config_sha, manifest_sha

    def test_manifest_config_platform_and_layer_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "image.tar"
            config, manifest = self.archive(path)
            actual = oci.archive_identity(path)
            self.assertEqual(actual["config"], "sha256:" + config)
            self.assertEqual(actual["manifest"], "sha256:" + manifest)
            self.assertEqual(actual["architecture"], "arm64")
            self.archive(path, corrupt=True)
            with self.assertRaises(AssertionError):
                oci.archive_identity(path)

    def test_unsupported_platform_and_root_identity_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "image.tar"
            for architecture, user in [("386", "65532:65532"), ("amd64", "0:0")]:
                self.archive(path, architecture=architecture, user=user)
                with self.assertRaises(AssertionError):
                    oci.archive_identity(path)


if __name__ == "__main__":
    unittest.main()
