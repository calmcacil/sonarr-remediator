#!/usr/bin/env python3
"""Check the tested release source against current upstream Go build inputs."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tomllib
from urllib.error import URLError
from urllib.request import Request, urlopen

from ci_common import digest, require


def go(source, *args):
    env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off", GOFLAGS="",
               GOPROXY="https://proxy.golang.org", GONOPROXY="none",
               GOSUMDB="sum.golang.org", GONOSUMDB="none")
    return subprocess.check_output(["go", *args], cwd=source, env=env, text=True, timeout=90)


def check_modules(source, manifest):
    require(not manifest.get("Replace") and not manifest.get("Exclude"),
            "release Go graph must not replace or exclude modules")
    names = sorted(item["Path"] for item in manifest["Require"])
    require(names, "release Go dependencies are missing")
    # go list emits a stream of pretty-printed JSON objects.
    payload = go(source, "list", "-mod=readonly", "-m", "-u", "-json", *names).lstrip()
    modules = {}
    decoder = json.JSONDecoder()
    while payload:
        entry, end = decoder.raw_decode(payload)
        modules[entry["Path"]] = entry
        payload = payload[end:].lstrip()
    require(set(modules) == set(names), "incomplete Go dependency listing")

    sqlite = modules["modernc.org/sqlite"]
    sqlite_mod = json.loads(go(source, "mod", "edit", "-json", sqlite["GoMod"]))
    # SQLite publishes a generated binding validated against one exact libc version.
    libc = modules["modernc.org/libc"]["Version"]
    expected_libc = next(item["Version"] for item in sqlite_mod["Require"] if item["Path"] == "modernc.org/libc")
    blockers = []
    if libc != expected_libc:
        blockers.append(f"SQLite {sqlite['Version']} requires libc {expected_libc}; selected {libc}")
    for name, module in modules.items():
        if module.get("Retracted"):
            blockers.append(f"{name} {module['Version']} is retracted")
        if name != "modernc.org/libc" and module.get("Update"):
            blockers.append(f"{name}: {module['Version']} -> {module['Update']['Version']}")
    require(not blockers, "Go dependency freshness blocked:\n  " + "\n  ".join(blockers))
    print(f"Go modules: {len(modules)} pins current, including SQLite {sqlite['Version']} with its required libc {libc}")


def check_builder(source, version):
    require(re.fullmatch(r"\d+\.\d+\.\d+", version), "Go pin must be a stable patch version")
    with urlopen("https://go.dev/dl/?mode=json", timeout=15) as response:
        stable = [entry["version"].removeprefix("go") for entry in json.load(response) if entry["stable"]]
    latest = max(stable, key=lambda v: tuple(map(int, v.split("."))))
    require(version == latest, f"Go toolchain: {version} -> {latest}")
    require(go(source, "env", "GOVERSION").strip() == f"go{version}", f"use mise-managed Go {version}")
    require(tomllib.loads((source / "mise.toml").read_text())["tools"]["go"] == version,
            "mise and go.mod must pin the same Go version")
    with urlopen("https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/golang:pull",
                 timeout=15) as response:
        token = json.load(response)["token"]
    request = Request(f"https://registry-1.docker.io/v2/library/golang/manifests/{version}-alpine", method="HEAD",
                      headers={"Accept": "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"})
    request.add_unredirected_header("Authorization", f"Bearer {token}")
    with urlopen(request, timeout=15) as response:
        latest_builder = digest(response.headers.get("Docker-Content-Digest"))
    for path in ("Containerfile", "scripts/FakeContainerfile"):
        expected = rf"^FROM docker\.io/library/golang:{re.escape(version)}-alpine\d+\.\d+@{latest_builder} AS build$"
        require(re.search(expected, (source / path).read_text(), re.MULTILINE),
                f"{path}: use the current Go {version} Alpine builder ({latest_builder})")
    print(f"Go toolchain and builder: {version}, {latest_builder}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", nargs="?", type=Path, default=Path.cwd())
    parser.add_argument("--part", choices=("all", "modules", "builder"), default="all")
    args = parser.parse_args()
    manifest = json.loads(go(args.source, "mod", "edit", "-json"))
    if args.part in ("all", "modules"):
        check_modules(args.source, manifest)
    if args.part in ("all", "builder"):
        check_builder(args.source, manifest["Go"])


if __name__ == "__main__":
    try:
        main()
    except (OSError, URLError, ValueError, KeyError, StopIteration, subprocess.SubprocessError) as error:
        raise SystemExit(f"Dependency freshness not verified: {error}") from None
