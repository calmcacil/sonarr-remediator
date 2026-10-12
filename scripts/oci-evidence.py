#!/usr/bin/env python3
"""Validate/export metadata for an exact tested OCI archive; never rebuild it."""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import subprocess
import tarfile


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def archive_identity(path):
    with tarfile.open(path) as archive:
        members = {m.name.removeprefix("./"): m for m in archive.getmembers()}

        def read(name):
            member = members[name]
            assert member.isfile(), "OCI metadata must be regular files"
            return archive.extractfile(member).read()

        def blob(digest):
            algorithm, value = digest.split(":", 1)
            assert algorithm == "sha256" and len(value) == 64
            data = read("blobs/sha256/" + value)
            assert hashlib.sha256(data).hexdigest() == value, "OCI blob digest mismatch"
            return data

        index = json.loads(read("index.json"))
        assert len(index["manifests"]) == 1, "platform archive must contain one manifest"
        descriptor = index["manifests"][0]
        manifest = json.loads(blob(descriptor["digest"]))
        assert manifest["mediaType"] == "application/vnd.oci.image.manifest.v1+json"
        config = json.loads(blob(manifest["config"]["digest"]))
        for layer in manifest["layers"]:
            blob(layer["digest"])
        assert config["os"] == "linux" and config["architecture"] in ("amd64", "arm64")
        assert config["config"]["User"] == "65532:65532"
        return {
            "manifest": descriptor["digest"], "config": manifest["config"]["digest"],
            "architecture": config["architecture"], "os": config["os"],
            "version": config["config"]["Labels"]["org.opencontainers.image.version"],
            "revision": config["config"]["Labels"]["org.opencontainers.image.revision"],
        }


def source():
    event = json.loads(pathlib.Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    return {
        "repository_id": event["repository"]["id"], "repository": event["repository"]["full_name"],
        "run_id": int(os.environ["GITHUB_RUN_ID"]), "run_attempt": int(os.environ["GITHUB_RUN_ATTEMPT"]),
        "pr_number": event["number"], "head": event["pull_request"]["head"]["sha"],
        "base": event["pull_request"]["base"]["sha"], "checkout": git("rev-parse", "HEAD"),
        "tree": git("rev-parse", "HEAD^{tree}"), "workflow": ".github/workflows/ci.yml",
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["platform", "gate"])
    parser.add_argument("--directory", default="artifacts")
    args = parser.parse_args()
    root = pathlib.Path(args.directory)
    identity = source()
    version = pathlib.Path("VERSION").read_text().strip()
    if args.mode == "platform":
        image = root / "image.oci.tar"
        runtime = json.loads((root / "runtime.json").read_text())
        oci = archive_identity(image)
        assert oci["config"] == runtime["image_config"]
        assert oci["architecture"] == runtime["architecture"]
        assert oci["version"] == runtime["version"] == version
        assert oci["revision"] == identity["checkout"]
        assert runtime["rootless"] and runtime["uid"] != 0
        assert runtime["shutdown_seconds"] < 25 and len(runtime["checks"]) >= 12
        (root / "metadata.json").write_text(json.dumps({
            "source": identity, "image": oci, "runtime": runtime,
            "archive_sha256": hashlib.sha256(image.read_bytes()).hexdigest(),
        }, indent=2) + "\n")
    else:
        assert os.environ["GO_RESULT"] == os.environ["PODMAN_RESULT"] == "success"
        platforms = {}
        for architecture in ("amd64", "arm64"):
            directory = root / architecture
            metadata = json.loads((directory / "metadata.json").read_text())
            archive = directory / "image.oci.tar"
            assert metadata["source"] == identity, "mixed source/run/attempt artifacts"
            assert metadata["image"] == archive_identity(archive)
            assert metadata["image"]["architecture"] == architecture
            assert metadata["image"]["version"] == version
            assert hashlib.sha256(archive.read_bytes()).hexdigest() == metadata["archive_sha256"]
            assert metadata["runtime"]["rootless"] and metadata["runtime"]["architecture"] == architecture
            metadata["artifact_name"] = f"image-{architecture}-{identity['run_id']}-{identity['run_attempt']}"
            platforms[architecture] = metadata
        now = datetime.datetime.now(datetime.timezone.utc)
        (root / "receipt.json").write_text(json.dumps({
            "source": identity, "version": version, "outcomes": {"go": "success", "podman": "success"},
            "created": now.isoformat(), "expires": (now + datetime.timedelta(days=5)).isoformat(),
            "platforms": platforms,
        }, indent=2) + "\n")


if __name__ == "__main__":
    main()
