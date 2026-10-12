#!/usr/bin/env python3
"""Bind native runtime results to exact OCI bytes and GitHub artifact identities."""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import subprocess
import tarfile

from ci_common import (ARCHITECTURES, GitHub, RUNTIME_CHECKS, WORKFLOW, digest, now,
                       one_artifact, require, sha, timestamp, validate_artifact, validate_source, version_parts)


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def file_hash(path):
    with pathlib.Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def archive_identity(path):
    with tarfile.open(path) as archive:
        members = {}
        for member in archive:
            name = member.name.removeprefix("./")
            require(name not in members, "duplicate OCI archive member")
            require(member.isfile() or member.isdir(), "OCI archive contains a link or special file")
            members[name] = member

        def read(name, limit=16 * 1024 * 1024):
            member = members[name]
            require(member.isfile() and member.size <= limit, "invalid OCI metadata file")
            return archive.extractfile(member).read()

        def blob(descriptor, metadata=True):
            identity = digest(descriptor["digest"])
            member = members["blobs/sha256/" + identity.removeprefix("sha256:")]
            require(member.isfile() and member.size == descriptor["size"], "OCI blob size mismatch")
            with archive.extractfile(member) as stream:
                require(hashlib.file_digest(stream, "sha256").hexdigest() == identity[7:], "OCI blob digest mismatch")
            return read(member.name.removeprefix("./")) if metadata else None

        require(json.loads(read("oci-layout"))["imageLayoutVersion"] == "1.0.0", "unsupported OCI layout")
        index = json.loads(read("index.json"))
        require(index["schemaVersion"] == 2 and len(index["manifests"]) == 1, "platform archive must contain one manifest")
        descriptor = index["manifests"][0]
        manifest = json.loads(blob(descriptor))
        require(manifest["schemaVersion"] == 2 and manifest["mediaType"] == "application/vnd.oci.image.manifest.v1+json",
                "archive must contain an OCI image manifest")
        config = json.loads(blob(manifest["config"]))
        for layer in manifest["layers"]:
            blob(layer, metadata=False)
        require(config["os"] == "linux" and config["architecture"] in ARCHITECTURES, "unsupported image platform")
        require(config["config"]["User"] == "65532:65532", "image must run as the application user")
        version = config["config"]["Labels"]["org.opencontainers.image.version"]
        version_parts(version)
        return {
            "manifest": descriptor["digest"], "config": manifest["config"]["digest"],
            "manifest_size": descriptor["size"], "layers": [layer["digest"] for layer in manifest["layers"]],
            "architecture": config["architecture"], "os": config["os"], "version": version,
            "revision": config["config"]["Labels"]["org.opencontainers.image.revision"],
            "source": config["config"]["Labels"]["org.opencontainers.image.source"],
        }


def source(api):
    event = json.loads(pathlib.Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    require(os.environ["GITHUB_EVENT_NAME"] == "pull_request", "evidence requires a PR event")
    require(event["pull_request"]["base"]["ref"] == "main", "PR must target main")
    repository = event["repository"]
    require(repository["full_name"] == api.repository, "event repository mismatch")
    checkout = sha(git("rev-parse", "HEAD"))
    require(checkout == os.environ["GITHUB_SHA"], "checkout does not match workflow revision")
    require(git("rev-parse", "HEAD^1") == event["pull_request"]["base"]["sha"] and
            git("rev-parse", "HEAD^2") == event["pull_request"]["head"]["sha"], "checkout is not the current PR merge ref")
    identity = {
        "repository_id": repository["id"], "repository": repository["full_name"],
        "run_id": int(os.environ["GITHUB_RUN_ID"]), "run_attempt": int(os.environ["GITHUB_RUN_ATTEMPT"]),
        "pr_number": event["number"], "head": sha(event["pull_request"]["head"]["sha"]),
        "base": sha(event["pull_request"]["base"]["sha"]), "checkout": checkout,
        "tree": sha(git("rev-parse", "HEAD^{tree}")), "workflow": WORKFLOW,
        "workflow_id": api.get("actions/runs/" + os.environ["GITHUB_RUN_ID"])["workflow_id"],
    }
    validate_source(identity)
    return identity


def validate_platform(directory, identity, version, architecture):
    metadata = json.loads((directory / "metadata.json").read_text())
    validate_source(metadata["source"])
    require(metadata["source"] == identity, "mixed source/run/attempt artifacts")
    require(metadata["image"] == archive_identity(directory / "image.oci.tar"), "OCI identity mismatch")
    require(metadata["archive_sha256"] == file_hash(directory / "image.oci.tar"), "archive hash mismatch")
    image = metadata["image"]
    require(image["architecture"] == architecture and image["version"] == version, "platform/version mismatch")
    require(image["revision"] == identity["checkout"], "image source mismatch")
    require(image["source"] == "https://github.com/" + identity["repository"], "image repository mismatch")
    validate_runtime(metadata["runtime"], image)
    require(json.loads((directory / "runtime.json").read_text()) == metadata["runtime"], "runtime file mismatch")
    return metadata


def validate_runtime(runtime, image):
    require(runtime["rootless"] is True and type(runtime["uid"]) is int and runtime["uid"] > 0,
            "runtime must be rootless")
    require(runtime["architecture"] == image["architecture"], "runtime platform mismatch")
    require(runtime["image_config"] == image["config"] and runtime["version"] == image["version"],
            "runtime exercised another image")
    require(type(runtime["shutdown_seconds"]) in (int, float) and 0 <= runtime["shutdown_seconds"] < 25,
            "shutdown deadline failed")
    require(set(runtime["checks"]) == RUNTIME_CHECKS, "missing or unknown runtime acceptance checks")
    require(bool(runtime["podman_version"]) and bool(runtime["storage_driver"]), "missing rootless engine identity")


def make_receipt(root, identity, version, outcomes, artifacts, current_time=None):
    validate_source(identity)
    require(outcomes == {"go": "success", "podman": "success"}, "all required jobs must succeed")
    current_time = current_time or now()
    platforms = {}
    for architecture in ARCHITECTURES:
        metadata = validate_platform(root / architecture, identity, version, architecture)
        name = f"image-{architecture}-{identity['run_id']}-{identity['run_attempt']}"
        metadata["artifact"] = validate_artifact(one_artifact(artifacts, name), name, identity["run_id"],
                                                  identity["head"], current_time)
        platforms[architecture] = metadata
    return {"schema": 1, "source": identity, "version": version, "outcomes": outcomes,
            "created": current_time.isoformat(),
            "expires": min(min(timestamp(p["artifact"]["expires_at"]),
                               timestamp(p["artifact"]["created_at"]) + datetime.timedelta(days=5))
                           for p in platforms.values()).isoformat(),
            "platforms": platforms}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["platform", "gate"])
    parser.add_argument("--directory", default="artifacts")
    args = parser.parse_args()
    root = pathlib.Path(args.directory)
    api = GitHub()
    identity = source(api)
    version = pathlib.Path("VERSION").read_text().strip()
    version_parts(version)
    if args.mode == "platform":
        image = root / "image.oci.tar"
        runtime = json.loads((root / "runtime.json").read_text())
        oci = archive_identity(image)
        require(oci["version"] == version and oci["revision"] == identity["checkout"], "image build identity mismatch")
        require(oci["source"] == "https://github.com/" + identity["repository"], "image repository mismatch")
        validate_runtime(runtime, oci)
        metadata = {"source": identity, "image": oci, "runtime": runtime, "archive_sha256": file_hash(image)}
        (root / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    else:
        artifacts = api.pages(f"actions/runs/{identity['run_id']}/artifacts", "artifacts")
        receipt = make_receipt(root, identity, version,
                               {"go": os.environ["GO_RESULT"], "podman": os.environ["PODMAN_RESULT"]}, artifacts)
        (root / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")


if __name__ == "__main__":
    main()
