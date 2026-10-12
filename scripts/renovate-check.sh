#!/usr/bin/env bash
set -euo pipefail
# Use the upstream release image; installing its npm graph hits a provenance downgrade.
SRA_RENOVATE_IMAGE=$(awk '/^FROM / {print $2}' scripts/RenovateContainerfile)
podman --cgroup-manager=cgroupfs --events-backend=file run --rm \
  --network=none --read-only --cap-drop=all --security-opt=no-new-privileges \
  --userns=keep-id --user=65532:65532 --tmpfs /tmp:rw,nosuid,size=128m \
  -v "$PWD/renovate.json:/config/renovate.json:ro" \
  --entrypoint renovate-config-validator "$SRA_RENOVATE_IMAGE" --strict /config/renovate.json
