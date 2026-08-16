# Dry-Run Validation Runbook

Before flipping `dryRun: false`, run the agent in its default dry-run mode and
validate that the actions it *would* take match what you actually want. This
runbook covers how to collect the key=value text logs (SPEC §9) and how to
read them so you can sign off on the recovery decisions before enabling
automation.

Dry-run is the safe default: `dryRun: true` in the config, or
`SRA_DRY_RUN=true` in the environment. In dry-run the agent detects and
evaluates every issue but never sends a mutating request to Sonarr — every
approved action is logged as `action.recommended` ("Would have …") instead of
`action.taken`. See SPEC §3.8.

---

## Table of Contents

1. [Confirm dry-run is active](#1-confirm-dry-run-is-active)
2. [Run it](#2-run-it)
3. [Collect the logs](#3-collect-the-logs)
4. [The lines that matter](#4-the-lines-that-matter)
5. [Anatomy of an `action.recommended` line](#5-anatomy-of-an-actionrecommended-line)
6. [Filtering and summarizing](#6-filtering-and-summarizing)
7. [Validation checklist](#7-validation-checklist)
8. [From validation to live](#8-from-validation-to-live)

---

## 1. Confirm dry-run is active

Set the flag one of two ways (environment wins over the file):

```
# docker-compose.yaml
environment:
  - SRA_DRY_RUN=true

# or config.yaml
dryRun: true
```

Prove it from the logs rather than trusting the config:

- Search your log for `dry_run=true`. Every `action.recommended` line carries
  it. If the flag is off you will see `type=action.taken` instead.
- A healthy dry-run run emits **only** `action.recommended` and
  `reconcile.plan` lines for approved decisions — never `action.taken`. If you
  ever see `type=action.taken`, dry-run is NOT active; stop and fix the flag
  before drawing any conclusions.

## 2. Run it

**Docker Compose (recommended for validation):** bring up the stack with the
compose file from DOCKER_SPEC.md §6 (`SRA_DRY_RUN=true` already set) and let it
poll for a representative window. Use `restart: unless-stopped` so it keeps
running while you observe.

**Local binary:**

```
make run                 # go run ./cmd/sonarr-remediator --config config.example.yaml
# or a built binary:
SRA_SONARR__URL=http://sonarr:8989 SRA_SONARR__API_KEY=… SRA_DRY_RUN=true \
  ./sonarr-remediator --config config.yaml
```

The default `logging.level` is `info`, which is enough for validation. Set
`SRA_LOGGING__LEVEL=debug` to also see every queue evaluation and detection
detail under `component=queue_monitor` / `component=detector` — useful when a
recommendation looks surprising and you want to see *why* a detector fired.

## 3. Collect the logs

Logs are key=value text on stderr; with Docker, `docker logs` is the surface.

```
# Live tail
docker logs -t -f sonarr-remediator

# Capture a window (the -t adds Docker's wall-clock timestamp; the agent's own
# UTC time= field is already on every line)
docker logs -t --since 12h sonarr-remediator > dryrun.log 2>&1
```

How long to run: long enough to cover your longest `waitHours`. The defaults
are `removeBrokenDownloads.waitHours: 6`, `removeTorrentErrors: 1`,
`resolveUnknownSeries: 1`, `removeNotCustomFormat: 2` (config schema: SPEC §8).
A 12–24 h window is enough to see every rule fire at least once against a busy
queue. Run `make docker-build` / `docker compose up -d` and stop with
`docker stop` (SIGTERM) so the shutdown log flush completes; a `docker kill`
can truncate the tail.

## 4. The lines that matter

| `type=` value | Meaning | Validation use |
|---|---|---|
| `action.recommended` | Approved action that dry-run would have taken | The core dataset — every would-be mutation |
| `reconcile.plan` | Episode reconciliation plan for one episode | Verify winner/discards are correct (SPEC §3.2) |
| `action.skipped` | Rejected by a safety check, with `reason` | Confirm rejections are expected (cooldown, exclusions) |
| `import.failed-all-retries` | All retries exhausted (`warn`) | Only appears in live retry runs; not in pure dry-run for new items |
| `error.sonarr-auth` / `error.sonarr-unreachable` | Connectivity/credentials (`error`) | Must be absent in a successful dry-run |

Routine detections that produce no action are plain `info` lines under
`component=queue_monitor` / `component=detector`; they are not events and are
safe to ignore for validation.

## 5. Anatomy of an `action.recommended` line

One line is emitted per approved decision. Field names below are exactly what
the executor writes (executor.go `decisionAttrs`):

```
time=2026-07-23T10:12:00.000Z level=INFO type=action.recommended msg="Would have removed queue item 420" decision_id=dec_abc123 item=key="42:105:abc123" id=420 title="Ubuntu" series="Ubuntu" episode="S01E05" release="Ubuntu.S01E05.1080p.WEB-DL" release_id=abc123 custom_format_score=120 custom_formats=[...] trigger=not_custom_format_upgrade checks=[{"check":"queue.status","expected":"completed","actual":"completed","passed":true},{"check":"age_hours","expected":">= 2","actual":"6.3","passed":true}] action=remove_queue message="Would have removed queue item 420" dry_run=true
```

| Token | What to check |
|---|---|
| `decision_id` | Stable ID for the decision; useful when correlating or reporting |
| `item=` group | `key` is `seriesId:episodeId:downloadId`; `series`/`episode` identify the target; `release`/`release_id` name the download; `custom_format_score`/`custom_formats` show scoring |
| `trigger` | Which detector fired: `stuck_download`, `not_custom_format_upgrade`, `import_failed`, `torrent_error`, `unknown_series`, `reconcile` |
| `checks` | Every safety gate with `expected`/`actual`/`passed` — verify the pass reasons are sane (statuses, ages) |
| `action` | What it would do: `remove_queue`, `manual_import`, `retry`, `reconcile`, `log_only` |
| `dry_run` | Always `true` in dry-run |

`reconcile.plan` and reconcile `action.recommended` lines additionally carry
`episode_key`, `upgrade` (whether the winner beats the existing file), and
`discards` (a JSON list of `{id, release, score}` for the releases the plan
would remove).

## 6. Filtering and summarizing

Pipe `dryrun.log` through standard text tools — the key=value shape is designed
for this (SPEC §9).

```
# Total recommendations
grep -c 'type=action.recommended' dryrun.log

# Recommendations by detector
grep 'type=action.recommended' dryrun.log | grep -o 'trigger=[a-z_]*' | sort | uniq -c

# Recommendations by action type
grep 'type=action.recommended' dryrun.log | grep -o 'action=[a-z_]*' | sort | uniq -c

# Reconciliation plans
grep 'type=reconcile.plan' dryrun.log

# Rejections and their reasons
grep 'type=action.skipped' dryrun.log | grep -o 'reason=[^ ]*' | sort | uniq -c

# Everything touching one series
grep 'type=action.recommended' dryrun.log | grep 'series="Ubuntu"'

# Any real mutations (should be zero in dry-run)
grep 'type=action.taken' dryrun.log
```

## 7. Validation checklist

- [ ] **No `action.taken` lines exist** — confirms dry-run is off-mutation.
- [ ] Every `trigger` maps to an issue class you recognize and actually want handled.
- [ ] Every `checks` entry shows `passed:true` with plausible `actual` values (statuses, ages, scores).
- [ ] Each `reconcile.plan` winner is genuinely the best release for the episode; `discards` are the ones you want removed; `upgrade` is correct.
- [ ] `action.skipped` reasons are expected (cooldown, exclusions) — not silent mis-detections.
- [ ] No recommendations touch series you never want touched → add them to `exclusions.seriesIds` / `exclusions.rootPaths`.
- [ ] `custom_format_score` and `upgrade` on reconcile lines match your upgrade intent.
- [ ] The window covered your longest `waitHours` so every enabled rule fired at least once.

If anything looks wrong, adjust the config (SPEC §8) — `waitHours`,
`blocklistRelease`, `minimumConfidence`, `exclusions`, per-rule `enabled` —
and re-run. `exclusions` is the fastest way to suppress a noisy series without
disabling a whole rule.

## 8. From validation to live

Once the recommendations match your intent:

1. Set `SRA_DRY_RUN=false` (or `dryRun: false` in the config) and redeploy.
2. Keep `exclusions` ready so a single series can be paused without a rebuild.
3. After going live, watch for `action.taken` and, importantly,
   `action.error` / `import.failed-all-retries` — these are the only lines
   that indicate something did not go as the dry-run predicted.
4. If a live decision is wrong, stop the container (SIGTERM), re-enable
   dry-run, and re-validate the changed config.

---

*Behavior and field shapes in this runbook are defined by SPEC.md (§3.8 dry
run, §7 safety, §9 logging) and DOCKER_SPEC.md (§1 runtime contract, §6
compose). To capture and submit logs from your own Sonarr for the maintainers
to analyze, see [DRYRUN_DATA_CONTRIB.md](DRYRUN_DATA_CONTRIB.md).*
