#!/usr/bin/env bash
set -euo pipefail
: "${SRA_ARCHITECTURE:?expected native architecture is required}"
: "${SRA_REVISION:?tested checkout revision is required}"
python -c 'import os, platform; assert os.getuid() != 0; assert {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()] == os.environ["SRA_ARCHITECTURE"]'
mkdir -p artifacts
podman --cgroup-manager=cgroupfs --events-backend=file info --format json > artifacts/podman-info.json
python -c 'import json; assert json.load(open("artifacts/podman-info.json"))["host"]["security"]["rootless"]'
SRA_BUILD_VERSION=$(cat VERSION)
# Podman 5.4.2 can reuse LABEL layers after build-argument changes.
podman --cgroup-manager=cgroupfs --events-backend=file build --no-cache --format oci \
  -f Containerfile --build-arg VERSION="$SRA_BUILD_VERSION" \
  --build-arg REVISION="$SRA_REVISION" -t localhost/remediator:ci .
python scripts/podman-smoke.py --image localhost/remediator:ci --architecture "$SRA_ARCHITECTURE"
podman --cgroup-manager=cgroupfs --events-backend=file save --format oci-archive \
  -o artifacts/image.oci.tar localhost/remediator:ci
python scripts/oci-evidence.py platform
