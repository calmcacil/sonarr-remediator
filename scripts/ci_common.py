"""Shared input validation and GitHub reads for CI and exact-image promotion."""
import datetime
import hashlib
import json
import os
import pathlib
import re
import shutil
import tempfile
import urllib.error
import urllib.parse
import urllib.request
import zipfile

ARCHITECTURES = ("amd64", "arm64")
WORKFLOW = ".github/workflows/ci.yml"
CHECK_APP_ID = 15368
RUNTIME_CHECKS = {
    "non-root", "private-state", "readonly-root-config", "no-ports-media-socket",
    "dry-run-zero-mutations", "local-queries", "health", "recommendation-deduplication",
    "restart-persistence", "bounded-shutdown", "config-permission-failure", "state-permission-failure",
    "manual-preview", "manual-one-off-dry-run", "manual-correlated-verification",
    "manual-receipt-restart", "manual-exit-codes", "manual-timeout",
}
VERSION_PATTERN = r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?"


def require(condition, message):
    # Assertions can disappear under PYTHONOPTIMIZE; release checks must not.
    if not condition:
        raise ValueError(message)


def version_parts(value):
    require(isinstance(value, str), "version must be a string")
    match = re.fullmatch(VERSION_PATTERN, value)
    require(match is not None and len(value) <= 100, "invalid OCI-compatible semantic version")
    major, minor, patch, pre = match.groups()
    if pre:
        require(all(not (p.isdigit() and len(p) > 1 and p[0] == "0") for p in pre.split(".")),
                "numeric prerelease identifiers cannot have leading zeros")
    return (int(major), int(minor), int(patch)), pre


def timestamp(value):
    result = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(result.tzinfo is not None, "timestamp must include timezone")
    return result


def now():
    return datetime.datetime.now(datetime.timezone.utc)


def sha(value):
    require(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{40}", value), "invalid Git identity")
    return value


def digest(value):
    require(isinstance(value, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", value), "invalid OCI digest")
    return value


def validate_source(value):
    for key in ("repository_id", "workflow_id", "run_id", "run_attempt", "pr_number"):
        require(type(value[key]) is int and value[key] > 0, "invalid source identity: " + key)
    for key in ("head", "base", "checkout", "tree"):
        sha(value[key])
    require(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", value["repository"]), "invalid source repository")
    require(value["workflow"] == WORKFLOW, "unexpected evidence workflow")


class GitHub:
    def __init__(self, repository=None):
        self.repository = repository or os.environ["GITHUB_REPOSITORY"]
        require(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", self.repository), "invalid repository name")
        self.token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
        self.prefix = f"repos/{self.repository}/"

    def request(self, path, method="GET", body=None):
        # Only GitHub receives this token; redirects/downloads use separate requests.
        require(not path.startswith(("/", "http")), "API path must be relative")
        headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request("https://api.github.com/" + path, data=data, headers=headers, method=method)
        with urllib.request.urlopen(request, timeout=60) as response:
            payload = response.read()
        return json.loads(payload) if payload else None

    def get(self, path):
        return self.request(self.prefix + path if path else self.prefix.rstrip("/"))

    def pages(self, path, key=None):
        result = []
        for page in range(1, 101):
            separator = "&" if "?" in path else "?"
            payload = self.get(f"{path}{separator}per_page=100&page={page}")
            items = payload[key] if key else payload
            require(isinstance(items, list), "API collection must be an array")
            result.extend(items)
            if len(items) < 100:
                return result
        raise ValueError("API pagination exceeded safety bound")

    def download(self, artifact_id, directory, expected_files, expected_digest):
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, request, fp, code, msg, headers, newurl):
                return None

        headers = {"Accept": "application/vnd.github+json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        request = urllib.request.Request(
            f"https://api.github.com/{self.prefix}actions/artifacts/{artifact_id}/zip", headers=headers)
        try:
            with urllib.request.build_opener(NoRedirect).open(request, timeout=60):
                raise ValueError("expected artifact download redirect")
        except urllib.error.HTTPError as error:
            with error:
                require(error.code == 302, "artifact download failed")
                location = error.headers["Location"]
        parsed = urllib.parse.urlparse(location)
        require(parsed.scheme == "https" and parsed.hostname and
                parsed.hostname.endswith((".blob.core.windows.net", ".githubusercontent.com")),
                "unexpected artifact download host")
        directory = pathlib.Path(directory)
        directory.mkdir(parents=True, exist_ok=True)
        # Follow the signed storage URL without forwarding the GitHub token.
        with tempfile.TemporaryFile() as temporary:
            hasher = hashlib.sha256()
            with urllib.request.urlopen(location, timeout=60) as response:
                size = 0
                while chunk := response.read(1024 * 1024):
                    size += len(chunk)
                    require(size <= 1024 ** 3, "artifact exceeds download bound")
                    temporary.write(chunk)
                    hasher.update(chunk)
            require("sha256:" + hasher.hexdigest() == digest(expected_digest), "GitHub artifact ZIP digest mismatch")
            temporary.seek(0)
            with zipfile.ZipFile(temporary) as archive:
                require(set(archive.namelist()) == set(expected_files) and
                        len(archive.namelist()) == len(expected_files), "unexpected artifact files")
                require(sum(member.file_size for member in archive.infolist()) <= 1024 ** 3,
                        "artifact exceeds expanded size bound")
                for name in expected_files:
                    with archive.open(name) as source, (directory / name).open("wb") as destination:
                        shutil.copyfileobj(source, destination)


def validate_artifact(artifact, name, run_id, head=None, current_time=None):
    require(artifact["name"] == name and type(artifact["id"]) is int and artifact["id"] > 0,
            "artifact identity mismatch")
    require(not artifact["expired"], "artifact expired")
    created = timestamp(artifact["created_at"])
    current_time = current_time or now()
    require(created <= current_time < min(timestamp(artifact["expires_at"]), created + datetime.timedelta(days=5)),
            "artifact outside five-day promotion window")
    workflow = artifact["workflow_run"]
    require(workflow["id"] == run_id, "artifact belongs to another run")
    if head:
        require(workflow["head_sha"] == head, "artifact belongs to another revision")
    digest(artifact["digest"])
    return {key: artifact[key] for key in ("id", "name", "created_at", "expires_at", "digest")}


def one_artifact(artifacts, name):
    matches = [a for a in artifacts if a["name"] == name]
    require(len(matches) == 1, "missing or ambiguous artifact: " + name)
    return matches[0]
