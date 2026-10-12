"""Controlled OCI/GitHub fixtures; no credentials or external services."""
import copy
import datetime
import hashlib
import io
import json
import tarfile

from ci_common import ARCHITECTURES, RUNTIME_CHECKS, WORKFLOW

TIME = datetime.datetime(2026, 10, 12, 12, tzinfo=datetime.timezone.utc)
SOURCE = {"repository_id": 123, "repository": "operator/service", "workflow": WORKFLOW,
          "workflow_id": 456, "run_id": 789, "run_attempt": 2, "pr_number": 12,
          "head": "a" * 40, "base": "b" * 40, "checkout": "c" * 40, "tree": "d" * 40}
REPOSITORY = {"id": 123, "full_name": "operator/service", "private": False, "default_branch": "main",
              "allow_squash_merge": True, "allow_merge_commit": False, "allow_rebase_merge": False}
PR = {"number": 12, "merged_at": TIME.isoformat(), "merge_commit_sha": "e" * 40,
      "head": {"sha": "a" * 40}, "base": {"ref": "main", "repo": {"id": 123}}}
RUN = {"id": 789, "run_attempt": 2, "event": "pull_request", "head_sha": "a" * 40,
       "repository": {"id": 123}, "workflow_id": 456, "path": WORKFLOW,
       "status": "completed", "conclusion": "success", "created_at": TIME.isoformat()}
JOBS = [{"name": name, "conclusion": "success", "run_id": 789, "check_run_url": "https://api.github.com/repos/operator/service/check-runs/80"}
        for name in ("go", "Podman (amd64)", "Podman (arm64)", "CI")]
EVENT = {"action": "published", "repository": {"id": 123},
         "release": {"draft": False, "prerelease": False, "tag_name": "v1.2.3"}}


def artifact(name, artifact_id):
    return {"name": name, "id": artifact_id, "expired": False, "created_at": TIME.isoformat(),
            "expires_at": (TIME + datetime.timedelta(days=5)).isoformat(), "digest": "sha256:" + "f" * 64,
            "workflow_run": {"id": 789, "head_sha": "a" * 40}}


def write_archive(path, architecture="arm64", user="65532:65532", corrupt=False, duplicate=False, version="1.2.3"):
    config = json.dumps({"os": "linux", "architecture": architecture, "config": {
        "User": user, "Labels": {"org.opencontainers.image.version": version,
                                  "org.opencontainers.image.revision": SOURCE["checkout"],
                                  "org.opencontainers.image.source": "https://github.com/" + SOURCE["repository"]}}}).encode()
    layer = b"controlled layer data"
    config_digest = "sha256:" + hashlib.sha256(config).hexdigest()
    layer_digest = "sha256:" + hashlib.sha256(layer).hexdigest()
    manifest = json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                           "config": {"digest": config_digest, "size": len(config)},
                           "layers": [{"digest": layer_digest, "size": len(layer)}]}).encode()
    manifest_digest = "sha256:" + hashlib.sha256(manifest).hexdigest()
    members = {"oci-layout": b'{"imageLayoutVersion":"1.0.0"}',
               "index.json": json.dumps({"schemaVersion": 2, "manifests": [{"digest": manifest_digest, "size": len(manifest)}]}).encode(),
               "blobs/sha256/" + config_digest[7:]: config,
               "blobs/sha256/" + layer_digest[7:]: b"tampered layer bytes" if corrupt else layer,
               "blobs/sha256/" + manifest_digest[7:]: manifest}
    with tarfile.open(path, "w") as archive:
        for name, data in members.items():
            member = tarfile.TarInfo(name)
            member.size = len(data)
            archive.addfile(member, io.BytesIO(data))
        if duplicate:
            member = tarfile.TarInfo("index.json")
            member.size = len(members["index.json"])
            archive.addfile(member, io.BytesIO(members["index.json"]))
    return {"manifest": manifest_digest, "config": config_digest, "manifest_size": len(manifest), "layers": [layer_digest],
            "architecture": architecture, "os": "linux", "version": version, "revision": SOURCE["checkout"],
            "source": "https://github.com/" + SOURCE["repository"]}


def platform(root, architecture):
    directory = root / architecture
    directory.mkdir()
    image = write_archive(directory / "image.oci.tar", architecture)
    runtime = {"architecture": architecture, "rootless": True, "uid": 1000, "podman_version": "5.4.2",
               "storage_driver": "overlay", "image_config": image["config"], "version": "1.2.3",
               "checks": sorted(RUNTIME_CHECKS), "shutdown_seconds": 0.2}
    metadata = {"source": copy.deepcopy(SOURCE), "image": image, "runtime": runtime,
                "archive_sha256": hashlib.sha256((directory / "image.oci.tar").read_bytes()).hexdigest()}
    (directory / "runtime.json").write_text(json.dumps(runtime))
    (directory / "metadata.json").write_text(json.dumps(metadata))
    return metadata


def platforms(root):
    return {architecture: platform(root, architecture) for architecture in ARCHITECTURES}


def artifacts():
    return [artifact("image-amd64-789-2", 101), artifact("image-arm64-789-2", 102), artifact("receipt-789-2", 103)]
