# Sonarr Recovery Agent

A sidecar service that runs next to Sonarr, watches its queue and history,
and automatically recovers from download and import problems that normally
require manual intervention. It talks only to the Sonarr API — no HTTP
server, no metrics endpoint, no notifications. Key=value text logs on
stderr, shaped `time= level= type= msg=` for easy filtering (e.g.
`type=action.taken`, `type=error.sonarr-auth`), are the only interface;
view them with `docker logs sonarr-remediator`.

Current release: v0.4.0 (alpha). Releases are cut with release-please and
published to `ghcr.io/calmcacil/sonarr-remediator`; see
[DOCKER_SPEC.md](DOCKER_SPEC.md) for the image tag scheme.

## What it does

The agent polls Sonarr's queue and history and detects these problem
classes:

- **Stuck downloads** — queue items older than `waitHours` that are not
  actively importing. Removed, optionally blocklisted, and re-searched.
- **"Not a custom format upgrade" removals** — downloads removed by Sonarr's
  upgrade policy. Detected from queue messages and history events; the
  release is removed and optionally blocklisted.
- **Torrent client errors** — items stuck in a qBittorrent-style error state
  that Sonarr never clears. Removed, blocklisted via the history endpoint,
  and re-searched.
- **Unknown-series downloads** — queue items with no series/episode (for
  example hash-titled items blocked with "Series title mismatch"). Resolved
  through Sonarr's manual-import preview first, removed only if the preview
  finds nothing.
- **Failed imports** — transient import errors (permission denied, missing
  files, timeouts, …) retried with a backoff schedule.
- **Reconciliation** — when multiple releases target the same episode, the
  one with the highest custom-format score is imported as an upgrade and the
  rest are removed.

## How it works

Monitors poll Sonarr (`GET /api/v3/queue`, history, system status) on
intervals from the config. Every issue is passed through the safety engine,
which checks dry-run mode, exclusions, age gates, per-download cooldowns,
and the "no other active item for the same episode" rule. Approved actions
run entirely through Sonarr's API (manual-import preview/command, queue
removal, history-failed, episode search); the agent never reads the shared
media filesystem. Every decision is logged as `action.taken` (or
`action.recommended` in dry-run), and a decision ring buffer is flushed at
shutdown so a full history is always visible in the logs.

All recovery runs through the manual-import preview flow and works on
Sonarr v3 and v4.

## Dry run

Dry-run mode is on by default (`dryRun: true`). The agent still detects and
evaluates issues but never mutates anything; each action it *would* take is
logged as `action.recommended`. Disable it with `SRA_DRY_RUN=false` or by
editing the config file.

## Quick start (Docker Compose)

1. Copy `docker-compose.example.yaml` and adjust the volumes for your setup.
2. Create the config directory, copy the example config, and make it
   readable by the runtime user (`PUID`/`PGID`, default 1000:1000):

   ```
   mkdir -p remediator-config
   cp config.example.yaml remediator-config/config.yaml
   chown -R "${PUID:-1000}:${PGID:-1000}" remediator-config
   ```

3. Set the Sonarr API key, either in the environment (`SONARR_API_KEY`) or
   directly on the `SRA_SONARR__API_KEY` line of the compose file.
4. Start: `docker compose up -d`

The container mounts the config directory read-only at `/config` (the media
mount at `/data` is retained for compatibility only). The container runs
read-only, and `--healthcheck` verifies the configuration and Sonarr
connectivity — `docker ps` shows the container's health.

## Configuration

The agent is configured with a YAML file (see `config.example.yaml` for the
full schema). Any key can be overridden with the `SRA_` prefix and double
underscores for nesting:

```
SRA_SONARR__URL=http://sonarr:8989
SRA_SONARR__API_KEY=abc123
SRA_DRY_RUN=false
SRA_LOGGING__LEVEL=debug
SRA_AUTOMATION__AUTO_MANUAL_IMPORT__ENABLED=true
```

## Building

`make build` builds the `sonarr-remediator` binary for linux/amd64 and
linux/arm64; `make docker-build` builds the container image. `make check`
runs the full local pipeline (format, vet, lint, tests, vulnerability
scan, workflow lint, doc-link check, docker builds).

## Reference

- [SPEC.md](SPEC.md) — application behavior: detection, safety checks, actions, configuration schema, action log.
- [DOCKER_SPEC.md](DOCKER_SPEC.md) — container packaging, Compose deployment, image tags, and lifecycle.
- [CHANGELOG.md](CHANGELOG.md) — release history.
