# Contributing Dry-Run Log Data

This guide is for **contributors** who want to help improve the agent's
detection and safety tuning. You run the agent in dry-run against your own
live Sonarr, capture the key=value text logs, redact anything private, and
commit the anonymized sample so the maintainers can analyze the decisions and
fine-tune the rules. Nothing is mutated in dry-run — this is read-only
observation.

Companion doc: [DRYRUN_VALIDATION.md](DRYRUN_VALIDATION.md) explains how to
*read* the lines once you have them.

---

## Table of Contents

1. [What we need](#1-what-we-need)
2. [Prerequisites](#2-prerequisites)
3. [Quick start (Docker)](#3-quick-start-docker)
4. [Capture the logs](#4-capture-the-logs)
5. [Redact and anonymize](#5-redact-and-anonymize)
6. [Submit the sample](#6-submit-the-sample)
7. [What makes a good sample](#7-what-makes-a-good-sample)

---

## 1. What we need

A time-bounded slice of dry-run logs (the `action.recommended` /
`reconcile.plan` / `action.skipped` lines) covering a busy enough period that
every enabled rule fired at least once. Alongside the log we need a short
metadata note: your Sonarr version, which rules were enabled and their
`waitHours`, and anything you think looked wrong. That's it — no API keys, no
live credentials, no changes to your library.

## 2. Prerequisites

- Docker + Docker Compose on a machine that can reach your Sonarr API.
- A Sonarr API key (`Settings → General → API Key` in the Sonarr UI).
- The agent and Sonarr reachable on the same network (the Compose `service`
  condition uses Sonarr's `/ping`, or just point `SRA_SONARR__URL` at your
  Sonarr).
- No other remediator already running against that Sonarr (avoid double
  observation).

## 3. Quick start (Docker)

Create a config directory and copy the example:

```bash
mkdir -p remediator-config
cp config.example.yaml remediator-config/config.yaml
chown -R "${PUID:-1000}:${PGID:-1000}" remediator-config
```

Run with dry-run forced on, debug logging on (so every detection shows), and
your Sonarr credentials:

```bash
docker run --rm -t \
  -v "$(pwd)/remediator-config:/config:ro" \
  -e SRA_SONARR__URL=http://sonarr:8989 \
  -e SRA_SONARR__API_KEY="${SONARR_API_KEY}" \
  -e SRA_DRY_RUN=true \
  -e SRA_LOGGING__LEVEL=debug \
  --name sonarr-remediator \
  ghcr.io/calmcacil/sonarr-remediator:latest
```

`SRA_DRY_RUN=true` is the safe default; the run above sets it explicitly so
there is no doubt. Confirm dry-run is active from the logs: every approved
action is logged as `action.recommended` with `dry_run=true`, and you should
**never** see `type=action.taken`. If you do, stop — dry-run is off.

Let it run long enough to cover your longest `waitHours`. Defaults are
`removeBrokenDownloads.waitHours: 6`, `removeTorrentErrors: 1`,
`resolveUnknownSeries: 1`, `removeNotCustomFormat: 2` (see
`config.example.yaml`). A **12–24 h** window is a good target so stuck-download
and abandoned-item rules have time to fire.

You can also point the existing `docker-compose.example.yaml` at this by
adding the same `SRA_DRY_RUN=true` / `SRA_LOGGING__LEVEL=debug` environment
entries and your `SONARR_API_KEY`.

## 4. Capture the logs

From another shell, save a window to a file (the `-t` adds Docker's
wall-clock timestamp; the agent's own UTC `time=` field is already on every
line):

```bash
docker logs -t --since 12h sonarr-remediator > dryrun-raw.log 2>&1
```

Stop the container with `docker stop sonarr-remediator` (SIGTERM) so the
shutdown log flush completes, then capture any tail if needed. If you prefer a
single shot, run the container with `--log-driver=local` or redirect its
output to a file from the start.

## 5. Redact and anonymize

The agent never logs the API key. But the logs *do* contain identifying
details you should strip before publishing:

| May appear in | Field / line | Action |
|---|---|---|
| Series title | `series=`, `title=` (in `item=` group) | Replace with a placeholder (`SERIES_A`) |
| Episode label | `episode=` (`S01E05`) | Replace or leave; not sensitive on its own |
| Release name | `release=` | Replace with a placeholder (`RELEASE_X`) |
| Sonarr host | `sonarr_url=` in `error.sonarr-*` lines, and `SRA_SONARR__URL` | Replace host with `http://sonarr:8989` |
| Series/episode IDs | `key="42:105:abc123"` | Replace the numeric IDs with placeholders if you want full anonymity |
| Download ID | `release_id=`, part of `key=` | Opaque hash; safe to leave |

A consistent find/replace is enough. For example, replace each real series
title with `SERIES_A`, `SERIES_B`, … and each release with `RELEASE_X`,
`RELEASE_Y`, … so relationships are still visible in the log. Keep the
mapping in your head/local notes only — do **not** commit the mapping.

Sanity-check the result:

```bash
grep -nE 'sonarr|apikey|token|password|http' dryrun-raw.log | less
```

If you are comfortable, ship the raw log and let the maintainers redact;
otherwise redact first. When in doubt, redact.

## 6. Submit the sample

1. Create a directory for your contribution:

   ```bash
   mkdir -p dryrun-samples
   cp dryrun-raw.log dryrun-samples/<your-handle>-<date>.log
   ```

   e.g. `dryrun-samples/alice-2026-08-16.log`. `dryrun-samples/` is tracked,
   so the file will be committed (unlike `config.live.yaml` /
   `config.local.yaml`, which are git-ignored).

2. Add a short metadata note in the same file's header or a sibling
   `<your-handle>-<date>.md`:

   ```markdown
   # Dry-run sample: alice-2026-08-16

   - Sonarr version: 4.0.0.741
   - Window: ~18h
   - Rules enabled: removeNotCustomFormat, removeBrokenDownloads,
     removeTorrentErrors, resolveUnknownSeries, reconcile
   - Config: defaults except removeBrokenDownloads.waitHours=4
   - Queue: ~40 items, mix of qBittorrent (torboxarr) and regular
   - Notes: reconcile winners looked correct; one not-custom-format removal
     seemed early (age 2h) — flagging for review
   ```

3. Commit and open a PR (or attach the file to an issue if you prefer):

   ```bash
   git add dryrun-samples/<your-handle>-<date>.log dryrun-samples/<your-handle>-<date>.md
   git commit -m "dryrun: add sample from alice (Sonarr 4.0.0.741)"
   git push
   ```

Do not commit `config.live.yaml`, `config.local.yaml`, or any file containing
your API key.

## 7. What makes a good sample

- **Covers the rules** — every enabled rule fired at least once (look for
  each `trigger=` value: `stuck_download`, `not_custom_format_upgrade`,
  `import_failed`, `torrent_error`, `unknown_series`, `reconcile`).
- **Long enough** — at least one full pass over your longest `waitHours`.
- **Annotated** — your metadata note says what looked right and what looked
  wrong; specific `decision_id` values help us correlate.
- **Anonymized** — no API key, no real hosts, series/release names replaced.
- **Reproducible config** — note the exact rule settings you ran, since
  defaults are what we tune against.

Samples like these are what let us validate detectors against real queues and
adjust safety thresholds, `waitHours`, and reconciliation scoring before a
release.

---

*Run/collect steps build on DOCKER_SPEC.md (§1 runtime contract, §6 compose)
and DRYRUN_VALIDATION.md (line interpretation, validation checklist).*
