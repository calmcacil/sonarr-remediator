#!/usr/bin/env python3
"""Verify merged-PR evidence, then promote exact OCI bytes without executing images."""
import argparse
import base64
import datetime
import hashlib
import importlib.util
import json
import os
import pathlib
import subprocess
import tempfile
import urllib.error
import urllib.parse
import urllib.request

from ci_common import (ARCHITECTURES, CHECK_APP_ID, GitHub, WORKFLOW, digest, now,
                       one_artifact, require, sha, timestamp, validate_artifact, validate_source, version_parts)

spec = importlib.util.spec_from_file_location("oci", pathlib.Path(__file__).with_name("oci-evidence.py"))
oci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(oci)


def validate_release(event):
    require(event["action"] == "published" and not event["release"]["draft"], "release must be explicitly published")
    tag = event["release"]["tag_name"]
    require(tag.startswith("v"), "release tag must start with v")
    version = tag[1:]
    _, prerelease = version_parts(version)
    require(event["release"]["prerelease"] is bool(prerelease), "release prerelease flag/version mismatch")
    return tag, version


def select_pr(prs, commit, repository_id):
    matches = [pr for pr in prs if pr.get("merged_at") and pr.get("merge_commit_sha") == commit
               and pr["base"]["ref"] == "main" and pr["base"]["repo"]["id"] == repository_id]
    require(len(matches) == 1, "release must identify one merged PR targeting this repository's main")
    return matches[0]


def select_run(runs, pr, repository_id, workflow_id):
    matches = [run for run in runs if run["event"] == "pull_request" and run["head_sha"] == pr["head"]["sha"]
               and run["repository"]["id"] == repository_id and run["workflow_id"] == workflow_id
               and run["path"] == WORKFLOW]
    require(bool(matches), "merged PR has no matching CI run")
    # A newer failed/pending run cannot be bypassed by choosing an older green run.
    matches.sort(key=lambda run: (timestamp(run["created_at"]), run["id"]), reverse=True)
    run = matches[0]
    require(run["status"] == "completed" and run["conclusion"] == "success", "current CI run did not succeed")
    return run


def validate_receipt(receipt, repository, pr, run, tree, version, jobs, artifacts, current_time=None):
    current_time = current_time or now()
    source = receipt["source"]
    validate_source(source)
    require(receipt["schema"] == 1, "unsupported receipt schema")
    expected = {
        "repository_id": repository["id"], "repository": repository["full_name"],
        "workflow": WORKFLOW, "workflow_id": run["workflow_id"], "run_id": run["id"],
        "run_attempt": run["run_attempt"], "pr_number": pr["number"], "head": pr["head"]["sha"],
        "tree": tree,
    }
    require(all(source[key] == value for key, value in expected.items()), "receipt repository/PR/run/source/tree mismatch")
    sha(source["base"])
    sha(source["checkout"])
    require(receipt["version"] == version, "release and receipt versions differ")
    require(receipt["outcomes"] == {"go": "success", "podman": "success"}, "receipt lacks successful prerequisites")
    require(timestamp(receipt["created"]) <= current_time < timestamp(receipt["expires"]), "receipt expired or from the future")
    require(current_time - timestamp(receipt["created"]) < datetime.timedelta(days=5), "receipt older than five days")
    require(timestamp(receipt["created"]) >= timestamp(run["created_at"]), "receipt predates its workflow run")
    expected_names = {"go", "Podman (amd64)", "Podman (arm64)", "CI"}
    require({job["name"] for job in jobs} == expected_names and len(jobs) == 4,
            "unexpected or missing CI prerequisite jobs")
    require(all(job["conclusion"] == "success" and job["run_id"] == run["id"] for job in jobs),
            "CI attempt contains failed/skipped/cancelled jobs")
    require(set(receipt["platforms"]) == set(ARCHITECTURES), "receipt must contain both native platforms")
    for architecture in ARCHITECTURES:
        metadata = receipt["platforms"][architecture]
        require(metadata["source"] == source, "platform from another revision/attempt")
        name = f"image-{architecture}-{run['id']}-{run['run_attempt']}"
        recorded = validate_artifact(one_artifact(artifacts, name), name, run["id"], source["head"], current_time)
        require(metadata["artifact"] == recorded, "artifact replaced or mixed with another attempt")
        image = metadata["image"]
        require(image["architecture"] == architecture and image["version"] == version and
                image["revision"] == source["checkout"], "image platform/version/revision mismatch")
        oci.validate_runtime(metadata["runtime"], image)
    return source


def prepare(api, event, directory):
    tag, version = validate_release(event)
    repository = api.get("")
    require(repository["id"] == event["repository"]["id"] and repository["full_name"] == api.repository,
            "release is for another repository")
    require(not repository["private"] and repository["default_branch"] == "main", "release requires public repository with main default")
    # Merge settings are omitted from read-only repository views; active rules are public.
    rules = api.get("rules/branches/main")
    require(isinstance(rules, list) and any(rule["type"] == "pull_request" and
            rule.get("parameters", {}).get("allowed_merge_methods") == ["squash"] for rule in rules),
            "main must enforce squash-only PR merges before release")
    commit_data = api.get("commits/" + urllib.parse.quote(tag, safe=""))
    commit = sha(commit_data["sha"])
    require(len(commit_data["parents"]) == 1, "release commit must be a squash merge")
    comparison = api.get(f"compare/{commit}...main")
    require(comparison["status"] in ("ahead", "identical"), "release commit is not reachable from main")
    tree = sha(commit_data["commit"]["tree"]["sha"])
    version_file = api.get("contents/VERSION?ref=" + commit)
    require(version_file["encoding"] == "base64" and base64.b64decode(version_file["content"]).decode().strip() == version,
            "tag does not match tagged VERSION")
    associations = api.pages(f"commits/{commit}/pulls")
    prs = [api.get(f"pulls/{pr['number']}") for pr in associations]
    pr = select_pr(prs, commit, repository["id"])
    workflow = api.get("actions/workflows/ci.yml")
    runs = api.pages("actions/workflows/ci.yml/runs?event=pull_request&head_sha=" + sha(pr["head"]["sha"]), "workflow_runs")
    run = select_run(runs, pr, repository["id"], workflow["id"])
    # Read current attempt explicitly. Artifacts from a previous attempt are not fallback evidence.
    run = api.get(f"actions/runs/{run['id']}")
    select_run([run], pr, repository["id"], workflow["id"])
    jobs = api.pages(f"actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs", "jobs")
    gate_jobs = [job for job in jobs if job["name"] == "CI"]
    require(len(gate_jobs) == 1, "CI gate job missing or ambiguous")
    gate_check_id = int(gate_jobs[0]["check_run_url"].rsplit("/", 1)[1])
    check = api.get(f"check-runs/{gate_check_id}")
    require(check["id"] == gate_check_id and check["name"] == "CI" and
            check["app"]["id"] == CHECK_APP_ID and check["conclusion"] == "success",
            "CI gate is not a successful GitHub Actions check")
    artifacts = api.pages(f"actions/runs/{run['id']}/artifacts", "artifacts")
    receipt_name = f"receipt-{run['id']}-{run['run_attempt']}"
    receipt_artifact = one_artifact(artifacts, receipt_name)
    validate_artifact(receipt_artifact, receipt_name, run["id"], pr["head"]["sha"])
    api.download(receipt_artifact["id"], directory, ["receipt.json"], receipt_artifact["digest"])
    receipt = json.loads((directory / "receipt.json").read_text())
    source = validate_receipt(receipt, repository, pr, run, tree, version, jobs, artifacts)
    tested_commit = api.get("git/commits/" + source["checkout"])
    require(tested_commit["tree"]["sha"] == tree and
            [parent["sha"] for parent in tested_commit["parents"]] == [source["base"], source["head"]],
            "recorded checkout is not the tested PR merge tree")
    for architecture in ARCHITECTURES:
        metadata = receipt["platforms"][architecture]
        api.download(metadata["artifact"]["id"], directory / architecture,
                     ["image.oci.tar", "metadata.json", "runtime.json"], metadata["artifact"]["digest"])
        actual = oci.validate_platform(directory / architecture, source, version, architecture)
        require(actual == {key: value for key, value in metadata.items() if key != "artifact"},
                "receipt differs from platform evidence")
    plan = {"repository": api.repository, "package": api.repository.lower(), "version": version, "tag": tag,
            "commit": commit, "tree": tree, "source": source, "platforms": receipt["platforms"],
            "prerelease": bool(version_parts(version)[1])}
    (directory / "plan.json").write_text(json.dumps(plan, indent=2) + "\n")
    return plan


class Registry:
    def __init__(self, package, authenticated=True):
        require(package == package.lower() and package.count("/") == 1, "invalid GHCR package")
        self.package = package
        headers = {}
        token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
        if authenticated and token:
            headers["Authorization"] = "Basic " + base64.b64encode(("x-access-token:" + token).encode()).decode()
        scope = "push,pull" if authenticated else "pull"
        url = "https://ghcr.io/token?" + urllib.parse.urlencode({"service": "ghcr.io", "scope": f"repository:{package}:{scope}"})
        with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=60) as response:
            self.token = json.load(response)["token"]

    def manifest(self, reference):
        require("/" not in reference, "invalid registry reference")
        request = urllib.request.Request(f"https://ghcr.io/v2/{self.package}/manifests/{reference}", headers={
            "Authorization": "Bearer " + self.token,
            "Accept": "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json",
        })
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                data = response.read()
                actual = "sha256:" + hashlib.sha256(data).hexdigest()
                require(response.headers["Docker-Content-Digest"] == actual, "registry returned inconsistent manifest")
                return actual, json.loads(data), data
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return None
            raise

    def put(self, reference, data):
        request = urllib.request.Request(f"https://ghcr.io/v2/{self.package}/manifests/{reference}", data=data, method="PUT", headers={
            "Authorization": "Bearer " + self.token, "Content-Type": "application/vnd.oci.image.index.v1+json",
        })
        with urllib.request.urlopen(request, timeout=60) as response:
            require(response.headers["Docker-Content-Digest"] == "sha256:" + hashlib.sha256(data).hexdigest(),
                    "published index identity mismatch")

    def verify_blob(self, expected):
        class StorageRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, request, fp, code, msg, headers, newurl):
                parsed = urllib.parse.urlparse(newurl)
                require(parsed.scheme == "https" and parsed.hostname and
                        parsed.hostname.endswith((".githubusercontent.com", ".blob.core.windows.net")),
                        "unexpected GHCR blob redirect host")
                return urllib.request.Request(newurl)

        digest(expected)
        request = urllib.request.Request(f"https://ghcr.io/v2/{self.package}/blobs/{expected}",
                                         headers={"Authorization": "Bearer " + self.token})
        hasher = hashlib.sha256()
        with urllib.request.build_opener(StorageRedirect).open(request, timeout=60) as response:
            size = 0
            while chunk := response.read(1024 * 1024):
                size += len(chunk)
                require(size <= 1024 ** 3, "registry blob exceeds size bound")
                hasher.update(chunk)
        require("sha256:" + hasher.hexdigest() == expected, "anonymous registry blob digest mismatch")


def immutable_preflight(registry, tag, expected):
    actual = registry.manifest(tag)
    require(actual is None or actual[0] == expected, "immutable GHCR tag collision: " + tag)
    return actual is not None


def command(*args, input=None):
    return subprocess.run(args, input=input, text=True, capture_output=True, check=True).stdout


def rootless():
    require(os.getuid() != 0, "publisher must be unprivileged")
    info = json.loads(command("podman", "--cgroup-manager=cgroupfs", "--events-backend=file", "info", "--format", "json"))
    require(info["host"]["security"]["rootless"] is True, "publisher Podman must be rootless")
    return info


def auth_file(directory):
    path = pathlib.Path(directory) / "auth.json"
    path.touch(mode=0o600)
    command("podman", "--cgroup-manager=cgroupfs", "--events-backend=file", "login", "--authfile", str(path),
            "--username", os.environ["GITHUB_ACTOR"], "--password-stdin", "ghcr.io",
            input=os.environ["GH_TOKEN"])
    return path


def publish_component(plan, architecture, directory, registry):
    info = rootless()
    require(info["host"]["arch"] == architecture, "publisher runner must match native architecture")
    image = plan["platforms"][architecture]["image"]
    tag = plan["tag"] + "-" + architecture
    if immutable_preflight(registry, tag, image["manifest"]):
        return image["manifest"]
    archive = directory / architecture / "image.oci.tar"
    require(oci.archive_identity(archive) == image, "component archive changed after verification")
    command("podman", "--cgroup-manager=cgroupfs", "--events-backend=file", "load", "--input", str(archive))
    loaded = json.loads(command("podman", "--cgroup-manager=cgroupfs", "--events-backend=file", "image", "inspect", image["config"]))[0]
    require("sha256:" + loaded["Id"].removeprefix("sha256:") == image["config"], "loaded image config changed")
    # Podman cannot require digest preservation. Skopeo fails instead of converting OCI bytes.
    with tempfile.TemporaryDirectory(prefix="sra-auth-") as private:
        auth = auth_file(private)
        command("skopeo", "copy", "--preserve-digests", "--authfile", str(auth),
                "oci-archive:" + str(archive), f"docker://ghcr.io/{plan['package']}:{tag}")
    require(registry.manifest(tag)[0] == image["manifest"], "registry component digest changed")
    return image["manifest"]


def index_bytes(plan):
    manifests = []
    for architecture in ARCHITECTURES:
        image = plan["platforms"][architecture]["image"]
        manifests.append({"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": digest(image["manifest"]),
                          "size": image["manifest_size"], "platform": {"architecture": architecture, "os": "linux"}})
    # Canonical bytes make the expected index digest reproducible before any write.
    return json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": manifests,
                       "annotations": {"org.opencontainers.image.version": plan["version"],
                                       "org.opencontainers.image.revision": plan["source"]["checkout"],
                                       "org.opencontainers.image.source": "https://github.com/" + plan["repository"],
                                       "com.github.actions.repository.id": str(plan["source"]["repository_id"]),
                                       "com.github.actions.run.id": str(plan["source"]["run_id"])}},
                      sort_keys=True, separators=(",", ":")).encode()


def should_advance_latest(registry, plan):
    if plan["prerelease"]:
        return False
    current = registry.manifest("latest")
    if current is None:
        return True
    manifest = current[1]
    require(manifest.get("mediaType") == "application/vnd.oci.image.index.v1+json", "unrecognized latest image; operator resolution required")
    annotations = manifest.get("annotations", {})
    require(annotations.get("org.opencontainers.image.source") == "https://github.com/" + plan["repository"],
            "latest belongs to an unrecognized source")
    require(annotations.get("com.github.actions.repository.id") == str(plan["source"]["repository_id"]),
            "latest belongs to another repository incarnation; operator resolution required")
    version = annotations.get("org.opencontainers.image.version")
    current_version, prerelease = version_parts(version)
    require(not prerelease, "existing latest is a prerelease; operator resolution required")
    immutable = registry.manifest("v" + version)
    require(immutable is not None and immutable[0] == current[0], "latest does not match its immutable stable version")
    if current_version == version_parts(plan["version"])[0]:
        require(current[0] == "sha256:" + hashlib.sha256(index_bytes(plan)).hexdigest(), "latest version digest collision")
        return False
    return current_version < version_parts(plan["version"])[0]


def publish_index(plan, registry):
    for architecture in ARCHITECTURES:
        image = plan["platforms"][architecture]["image"]
        actual = registry.manifest(plan["tag"] + "-" + architecture)
        require(actual is not None and actual[0] == image["manifest"], "missing or mismatched published architecture")
        require(actual[1]["config"]["digest"] == image["config"] and
                [layer["digest"] for layer in actual[1]["layers"]] == image["layers"], "published config/layers changed")
    data = index_bytes(plan)
    expected = "sha256:" + hashlib.sha256(data).hexdigest()
    # Resolve latest policy before writing; a failure cannot silently move latest.
    advance = should_advance_latest(registry, plan)
    if not immutable_preflight(registry, plan["tag"], expected):
        registry.put(plan["tag"], data)
    require(registry.manifest(plan["tag"])[0] == expected, "published version index mismatch")
    if advance:
        registry.put("latest", data)
        require(registry.manifest("latest")[0] == expected, "latest index mismatch")
    return expected, advance


def report(plan, identity, latest):
    message = (f"Published ghcr.io/{plan['package']}:{plan['tag']}@{identity}\n\n"
               f"Platforms: linux/amd64, linux/arm64. Latest advanced: {latest}.\n\n"
               f"Tested PR #{plan['source']['pr_number']}, CI run {plan['source']['run_id']} "
               f"attempt {plan['source']['run_attempt']}. Tested revision: {plan['source']['checkout']}. "
               f"Release squash commit: {plan['commit']}. Exact tree: {plan['tree']}.\n")
    print(message)
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as stream:
            stream.write(message)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["verify", "component", "index"])
    parser.add_argument("--architecture", choices=ARCHITECTURES)
    parser.add_argument("--directory", default="artifacts/release")
    args = parser.parse_args()
    event = json.loads(pathlib.Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    require(os.environ["GITHUB_EVENT_NAME"] == "release", "publisher requires a release event")
    directory = pathlib.Path(args.directory)
    directory.mkdir(parents=True, exist_ok=True)
    plan = prepare(GitHub(), event, directory)
    if args.mode == "verify":
        print(json.dumps({key: plan[key] for key in ("tag", "commit", "tree", "source")}, indent=2))
    elif args.mode == "component":
        require(args.architecture in ARCHITECTURES, "component architecture required")
        identity = publish_component(plan, args.architecture, directory, Registry(plan["package"]))
        print(f"Verified immutable {args.architecture} component: {identity}")
    else:
        rootless()
        identity, latest = publish_index(plan, Registry(plan["package"]))
        # Public repository visibility does not imply public GHCR visibility.
        anonymous = Registry(plan["package"], authenticated=False)
        require(anonymous.manifest(plan["tag"])[0] == identity, "anonymous public index pull failed")
        for architecture in ARCHITECTURES:
            image = plan["platforms"][architecture]["image"]
            require(anonymous.manifest(image["manifest"])[0] == image["manifest"], "anonymous platform pull failed")
            for blob in [image["config"], *image["layers"]]:
                anonymous.verify_blob(blob)
        report(plan, identity, latest)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as error:
        # Never print command arguments/input that might contain registry credentials.
        raise SystemExit(f"Container transfer failed (exit {error.returncode}): {error.stderr}") from None
