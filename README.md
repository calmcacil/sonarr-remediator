# Sonarr Remediator

A small Sonarr v4 recovery service. It observes completed downloads blocked on
import, asks Sonarr for automatic matches, and only requests an import when every
file passes policy. It uses Sonarr's API exclusively and mounts no media directory.

Dry-run is the default. Cleanup, release blocklisting, and replacement search are
separate permissions, all disabled by default. Cleanup always requests deletion
from the download client and can delete downloaded data.

## Quick Start

Install the project-pinned runtimes and check tools with mise, then build and test:

```sh
mise install
mise run check
mise run build
```

`check` also validates CI/release automation and Renovate configuration using
rootless Podman. See [CI, releases, and dependency updates](docs/CI.md) for required
repository policy, five-day image retention, and explicit release publishing.

Copy `config.example.yaml` to a private configuration directory, set `sonarr.url`,
and place the API key in `sonarr_api_key` inside that directory. Use the
[rootless Podman instructions](docs/OPERATIONS.md) for deployment and volume
permissions. The service opens no inbound port.

```sh
podman build -f Containerfile -t localhost/sonarr-remediator:0.4.0 .
mkdir -m 700 state
podman run -d --name sonarr-remediator \
  --userns=keep-id:uid=65532,gid=65532 \
  --read-only --cap-drop=all --security-opt=no-new-privileges \
  --stop-timeout=30 --health-cmd='/sonarr-remediator healthcheck' \
  --health-start-period=3m \
  -v "$PWD/config:/config:ro" -v "$PWD/state:/state" \
  localhost/sonarr-remediator:0.4.0 run --config /config/config.yaml
```

Attach the container to a private network where the configured Sonarr URL is
reachable. HTTPS uses bundled trust roots and normal certificate verification.

## Inspect Work

These commands read SQLite locally, need no API key, and send no Sonarr requests:

```sh
podman exec sonarr-remediator /sonarr-remediator status --json
podman exec sonarr-remediator /sonarr-remediator issues
podman exec sonarr-remediator /sonarr-remediator show DOWNLOAD_ID
podman exec sonarr-remediator /sonarr-remediator actions --download-id DOWNLOAD_ID --limit 20
podman exec sonarr-remediator /sonarr-remediator triggers --limit 20
```

Use Sonarr's opaque `downloadId`, not a hash in the release title. All query
commands accept `--state PATH`; listing commands accept `--limit`. Without a
subcommand the binary runs the service. `version` reports the application version.

## Request One Cycle

Preview or explicitly execute one bounded cycle through the running observer:

```sh
podman exec sonarr-remediator /sonarr-remediator trigger --dry-run --override-wait
podman exec sonarr-remediator /sonarr-remediator trigger --execute --override-wait --download-id DOWNLOAD_ID
podman exec sonarr-remediator /sonarr-remediator triggers --request-id REQUEST_ID --json
```

Preview is the default and does not persist state. `--execute` permits work for
that request even when background dry-run is enabled. `--override-wait` skips
the initial recovery/cleanup waits and episode cooldowns; permissions, retry
protection, competition and shared mutation budgets still apply. Repeat `--download-id` for
multiple exact IDs, or omit it to evaluate the current queue and pending searches.
With default budgets one cycle can attempt at most one mutation. Cleanup uses
the download client and can delete downloaded data.

The commands use a private local socket on the existing state mount. They need
the running observer and its state owner identity, with no new port or mount.
Use equivalent `docker exec` commands for Docker. See [Operations](docs/OPERATIONS.md)
for timeout, exit codes, receipts and the schema-2 rollback boundary.

## Policy

Completed `importBlocked`, `importFailed` and `importPending` downloads use the
same policy: recovery waits for 15 minutes of observed eligibility, and cleanup
waits six hours except for the permanent episode mismatches described below.
Both waits are configurable. Restarts and failed observation
gaps do not advance them. Pending downloads accumulate age instead of remaining
protected indefinitely; active `importing` and `imported` states are preserved.
Shared queue rows yield one decision per download.

Existing files are replaced only for a strict improvement under the current
Sonarr quality profile. Grouped qualities compare equally; cutoffs, minimum
custom-format scores, and proper/repack preference apply. An accepted preview
with equal quality and score does not authorize a replacement. Any rejected or
ambiguous media file defers import of the whole pack.

With cleanup enabled, a valid Sonarr import rejection that persists for
`cleanup.wait` (six hours by default) authorizes deletion of the shared download
and its downloaded data. This includes indeterminate samples, TheXEM mapping,
access failures and unknown rejection wording; one rejected file can qualify a
whole pack. The rejected-time clock resets when the blocker clears and excludes
downtime or failed reads. Existing library files are preserved. Empty, malformed
or unavailable previews do not authorize deletion.

Sonarr's explicit permanent rejection that an episode was unexpected for the
folder name identifies a failed download. With cleanup enabled, this specific
episode mismatch removes the shared download from the queue and download client
on its first eligible scan, without the six-hour wait. This can delete a whole
pack's downloaded data. Other rejection wording and temporary rejections keep
the configured wait. Scope, competition, cooldowns, mutation limits and uncertain
action protection still apply; reports show the mismatch and a zero wait.

Generic rejected-download cleanup never blocklists or searches again. The
separate explicit `Sample`/`No audio tracks detected` path can use those permissions
when enabled and fully proven. Redundancy cleanup also remains available for
equal/inferior candidates with acceptable library files for every intended episode.

The default mutation limits are one per scan and five per rolling hour, shared
by imports, removals, and searches. Intent and budget are committed before
transmission. Unknown outcomes remain protected across restarts and are never
blindly retried. `actions` distinguishes recommendations, acknowledgement,
verification, partial effects, and uncertainty.

The full approved behavior and architecture are in
[PRODUCT.md](specs/sonarr-remediator/PRODUCT.md) and
[TECH.md](specs/sonarr-remediator/TECH.md). Validation uses controlled fixtures;
activating against a live Sonarr is a separate operator decision.
