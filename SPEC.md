# Sonarr Recovery Agent — Project Specification

This specification describes the complete implementation. Current release: v0.4.0 (alpha).

---

## Table of Contents

1. [Overview](#1-overview)
2. [Goals & Non-Goals](#2-goals--non-goals)
3. [Core Features](#3-core-features)
4. [Architecture](#4-architecture)
5. [Component Specifications](#5-component-specifications)
6. [Data Models](#6-data-models)
7. [Safety](#7-safety)
8. [Configuration Schema](#8-configuration-schema)
9. [Logging & Action Log](#9-logging--action-log)
10. [Testing Strategy](#10-testing-strategy)
11. [Deployment](#11-deployment)
12. [Appendix: Sonarr API Surface](#12-appendix-sonarr-api-surface)

---

## 1. Overview

Sonarr Recovery Agent is a Go microservice that runs alongside Sonarr as a sidecar (container packaging: [DOCKER_SPEC.md](DOCKER_SPEC.md)). It autonomously detects, analyzes, and recovers from common download and import issues that normally require manual intervention.

It is **not** a cleanup script. It is a continuous recovery agent that observes Sonarr's state, evaluates configurable safety checks, and acts only when it is confident an action is safe.

### Key Characteristics

| Property | Value |
|---|---|
| Language | Go (latest stable; currently 1.26) |
| Runtime | Long-running Go process — packaged as a Docker sidecar (see [DOCKER_SPEC.md](DOCKER_SPEC.md)) |
| State | In-memory only (retries, decision ring buffer); all source data lives in Sonarr |
| Configuration | YAML file + environment variable overrides with strict validation |
| Observability | Key=value text logs to stderr (`time= level= type= msg=`) — the only output surface |
| Safety | Dry-run mode, safety checks gate every destructive action, every action (or recommended action) is logged |
| Interface | No HTTP server, no API, no metrics endpoint, no notifications — logs are the interface |

### Relationship with Sonarr

The agent interacts **only** with Sonarr's API. It never communicates with download clients directly. When a queue item needs to be removed, the agent tells Sonarr to remove it; Sonarr forwards the removal to the download client if configured. All recovery flows (import recovery, retries, reconciliation) run through Sonarr's manual-import preview and command endpoints, so the agent never reads the shared media filesystem; file paths are Sonarr's own.

At startup, the agent detects the running Sonarr version by parsing `GET /api/v3/system/status` (e.g. `"version": "4.0.0.741"` → major version 4) and adapts API call behavior where minor differences exist (blocklist parameter naming, event type values).

### Path Translation

Legacy note: earlier versions of the agent scanned the shared media filesystem
and translated paths between its own mount view and Sonarr's
(`paths.agentRoot`/`paths.sonarrRoot`, `paths.downloadRoots`). Recovery is now
filesystem-independent, and those configuration keys are parsed and validated
for compatibility only — they no longer affect behavior. Sonarr's preview
returns paths in Sonarr's own view, which are submitted unchanged.

---

## 2. Goals & Non-Goals

### Goals

- Reduce manual Sonarr maintenance.
- Automatically recover from common failure scenarios (stuck downloads, failed imports, naming mismatches).
- Remove downloads that will never import.
- Assist with edge-case manual imports via automatic recovery with confidence scoring.
- Operate completely autonomously once configured and dry-run is disabled.
- Produce a complete, human-readable record of every action taken and every action it would have taken.

### Non-Goals

- Direct download client management (the agent tells Sonarr; Sonarr tells the client).
- Replacing Sonarr's internal download decision logic.
- Acting as a media manager or indexer.
- Multi-instance coordination (single Sonarr per agent instance).
- Full CRUD management UI for Sonarr — no HTTP server, dashboard, or web UI of any kind.
- Metrics collection or any Prometheus/observability endpoint.
- Notifications of any kind (email, webhooks, chat) — structured logs are the only output.
- Persistent database (in-memory state is sufficient; Sonarr is the source of truth).
- Multi-language support (English only for status message matching and UI).
- Residual file cleanup (deleting samples, NFOs, empty folders, unpack dirs) — see §3.7.

---

## 3. Core Features

### 3.1 Queue Monitoring

Continuously polls and evaluates every item in the Sonarr download queue.

**Data sources:**

| Endpoint | Purpose | Interval |
|---|---|---|
| `/api/v3/queue` | Active and queued downloads (incl. unknown-series items: the fetch requests `includeUnknownSeriesItems=true`, since Sonarr's default hides items whose series is not in the library — exactly the import-blocked items a remediator must see) | 30 s |
| `/api/v3/system/status` | Sonarr health, connectivity, and version | 60 s |
| `/api/v3/history` | Completed/failed import history | On demand, per queue item |

History is not polled on a schedule. The queue monitor fetches history for a specific episode (`episodeId`) only when evaluating a queue item that needs it (abandoned-item checks, repeated import failures, not-custom-format confirmation, failed-import recovery).

**Tracked state per queue item (composite key: `seriesId:episodeId:downloadId`):**

- Queue status: one of `queued`, `paused`, `downloading`, `completed`, `warning`, `failed`
- Import state (`trackedDownloadState`): one of `downloading`, `importPending`, `importing`, `imported`, `importFailed`, `downloadFailed`
- Status messages: warning text, error text, `trackedDownloadStatus` (`ok`, `warning`, `error`)
- Duration in current state (age tracking)
- Series and episode identifiers for cross-referencing with history

---

### 3.2 Stuck Download Removal

Detects downloads that have become permanently stuck and will never import successfully.

**Eligible states:** Only items with queue status in `completed`, `warning`, or `failed` are evaluated. Items in `queued`, `paused`, or `downloading` are never acted upon.

**Trigger conditions (any single one is sufficient):**

| Condition | Detection |
|---|---|
| Sonarr reports error | `errorMessage` is set or `trackedDownloadStatus` = `error` |
| Missing files | Status message contains "No files found are eligible for import" |
| Abandoned item | `completed` status but no import attempt in history after configurable timeout (default 6 h) |
| Age timeout | Time since download completed > `maxAge` AND no import in progress |
| Repeated import failure | History shows N consecutive `downloadFailedImport` events for same episode |

**Actions (ordered, all optional):**

1. Remove from queue (`DELETE /api/v3/queue/{id}`)
2. Remove from queue with blocklist (`DELETE /api/v3/queue/{id}?blocklist=true`)
3. Log only (no mutation)

**Safety checks (see §7):**

- Item age >= configured minimum (default 2 h)
- Not currently in `importing` state
- No manual import scheduled for this item
- No active retry scheduled for this item
- Automation rule enabled
- Dry-run check

**Release context (enrichment, non-blocking):** every detection records the
release's identity and episode match in the issue details and decision log:
`release_id` (download id), `release_title`, `episode_id`, `episode_match`
(S/E + title when the episode resolves), `episode_has_file`, `existing_quality`
(when the episode already has a file), `custom_formats`, and
`custom_format_score`. Lookup failures are logged and dropped — they never
block or change a detection.

**Episode reconciliation (executed):** after each poll the queue monitor maps
every targeted hit (items that produced a stuck-download or not-custom-format
issue) by episode via `selector.Reconcile`. For each episode the release with
the highest `customFormatScore` is selected as the winner (ties: earliest
`added`, then input order); every other release for that episode is a
discard. Items without an episode match are left to the per-item flow.

Each plan is emitted as one `reconcile` issue (anchored on the winner, with
the full plan in the `reconcile_plan` details) and flows through the safety
engine and executor like any other action; per-item issues for plan-covered
items are suppressed so no item is acted on twice. The plan is also logged as
an informational `reconcile.plan` event.

Execution (`automation.reconcile.enabled`, default true) decides per plan:

1. **Upgrade check** (`selector.IsUpgrade`): the winner imports when it is an
   upgrade over the existing episode file — no file: always an upgrade;
   strictly higher `customFormatScore` than the existing file: upgrade;
   equal scores: only if the winner's quality is strictly better (unknown
   weights never upgrade).
2. **Import winner**: `recovery.ReconcileImport` — matching is Sonarr's
   job, exactly like the UI manual-import dialog. The queue item's download
   is previewed via
   `GET /api/v3/manualimport?downloadId=…&filterExistingFiles=false`
   (Sonarr derives the folder from the tracked download and anchors the
   series/episode match to the grab history; `filterExistingFiles=false` is
   required because every reconciled episode already has a media file and
   the default filter would drop the candidate). The file Sonarr matched to
   the episode — or the single file in a one-file folder — is then posted
   back as a `ManualImport` command (`POST /api/v3/command`, the same
   command the UI's Import button sends) with Sonarr's own quality,
   languages, and episode IDs. Sonarr executes the import and removes the
   tracked download on completion; the pipeline proves the import by
   polling `GET /api/v3/queue` until the item's ID disappears (bounded
   poll), never by the command's acknowledgement alone. A failed command
   (non-2xx) or a queue item that survives the poll window means the import
   did not commit: the pipeline reports it as not imported, logs an
   `action.skipped` event, and leaves the winner in the queue — it never
   logs a successful import that did not occur. The download folder never
   needs to be accessible to the agent's filesystem.
3. **Remove non-upgrade winner**: `DELETE /api/v3/queue/{id}?removeFromClient=true`.
4. **Remove every discard**: `DELETE /api/v3/queue/{id}?removeFromClient=true`,
   unconditionally — the plan has resolved them. Discards are processed even
   if the winner's import fails.

Dry-run logs the intended winner action (import/remove) and discard count as
`action.recommended` and performs no mutations; live mode executes the calls
above.

---

### 3.3 "Not a Custom Format Upgrade" Removal

Downloads that complete but Sonarr determines are not an upgrade based on custom format scoring.

**Detection strategy (both methods used; either match triggers detection):**

**Method A — Queue status message parsing:**
- Queue item has `trackedDownloadStatus` = `warning`
- Status message text matches the built-in regex: `(?i)not.*(custom format|an upgrade)`
- Or a configurable custom regex if provided

**Method B — History event inspection:**
- Primary: History contains `eventType` = `downloadIgnored` with data matching "Not an upgrade"
- Fallback (older versions): `eventType` = `downloadFailedImport` with matching status message

**Safety checks:**

| Condition | Source |
|---|---|
| Download completed | Queue status = `completed` |
| Import decision confirmed as no-upgrade | Status message OR history event |
| Age >= waitHours (default 2) | `automation.removeNotCustomFormat.waitHours` |
| Not currently importing | `trackedDownloadState` ≠ `importing` |
| No active retry scheduled for this item | In-memory retry queue |
| No other active queue item for same episode | Queue + episode ID cross-check |
| Rule enabled | `automation.removeNotCustomFormat.enabled` |

**Default action:** Remove from queue via Sonarr API. Blocklisting is optional via config.

---

### 3.4 Import Recovery

For downloads where the download completed but Sonarr could not automatically match and import the video file(s).

**Common failure causes:**

- Strange or obfuscated release names
- Download client renamed the folder
- Multi-file releases (CD1/CD2, sample + main, extras)
- Anime naming (handled via TVDB SxxExx parsing)
- Scene naming conventions Sonarr cannot parse
- Folder name / file name mismatch
- Unpacked output in unexpected subdirectories

**Recovery workflow (filesystem-independent; works on Sonarr v3 and v4):**

```
1. DETECT
   Queue item: trackedDownloadState = importFailed.
   History shows: eventType = downloadFailedImport.

2. PREVIEW
   GET /api/v3/manualimport?downloadId=…&filterExistingFiles=false
   (Sonarr derives the folder from the tracked download, anchors the
   series/episode match to the grab history, and reports the files it
   could match with its own quality, languages, and episode IDs — the
   same call the UI's manual-import dialog makes).

3. SELECT
   The file Sonarr matched to the expected episode; a single-file folder
   is unambiguous and also accepted. Folders with several files and no
   episode match are ambiguous → skip.

4. SCORE CONFIDENCE (0-100)
   - Sonarr matched episodes (series resolved): +35
   - Any matched episode is in the expected season: +25
   - Matched episodes contain the expected episode: +25
   - Quality recognized (non-zero quality ID): +10
   - Language recognized (non-empty languages): +5
   Total: max 100. A file Sonarr could not match scores 0 (skip).
   Confidence breakdown is logged.

5. PRE-IMPORT CHECK
   a. Call GET /api/v3/episode/{episodeId} to check episode status.
   b. If episode hasFile == false → no existing file, proceed to import.
   c. If episode hasFile == true → call GET /api/v3/episodefile/{episodeFileId}
      to retrieve the existing file's quality.
   d. Compare qualities: if the existing file's quality weight is >= the candidate
      quality weight, reject (log and skip). Quality weights are obtained from
      the `QualityDefinition` list fetched at startup (higher weight = better).
      The preview result's `QualityModel.ID` is matched to a `QualityDefinition`
      to get the weight. The existing file's quality is a name only; it is
      matched to a `QualityDefinition` by name to obtain a weight. If either
      lookup fails, compare by quality name as a fallback.
   e. For multi-episode files, check each episode individually. Skip episodes
      with equal or better files; import to remaining episodes.

6. IMPORT (if confidence >= threshold)
   For each qualifying episode, call a ManualImport command (POST
   /api/v3/command); one episode per file. Request body includes:
   - name: "ManualImport", importMode: "auto"
   - files[0].path (as returned by Sonarr's preview — Sonarr's view)
   - files[0].seriesId, files[0].episodeIds (one entry per call)
   - files[0].quality (from the preview), languages (from the preview)
   - files[0].downloadId (from queue item)
   Import mode is fixed to "auto"; Sonarr imports without a custom-format
   upgrade gate, matching the UI's Import button.
   The import is proven by polling GET /api/v3/queue until the item's ID
   disappears (bounded poll), never by the command's acknowledgement alone
   — a surviving item means the import did not commit and is logged as such,
   never as a success.

7. LOG
   Record action (or recommended action in dry-run) with full confidence breakdown.
```

**Auto-import threshold:**

- `confidence >= autoManualImport.minimumConfidence` (default 95): import automatically.
- `confidence < minimumConfidence`: log confidence breakdown, skip.

The pipeline needs no access to the shared media filesystem: every file path
is Sonarr's own, and all matching is done by Sonarr. The `paths` configuration
keys remain parsed and validated for compatibility but do not affect this
workflow.

---

### 3.5 Import Recovery Result

When the recovery engine (see §3.4) finds a candidate file but confidence is below the auto-import threshold, the result is logged with full confidence breakdown (TVDB match, season match, episode match, quality known, language known) and the item proceeds to retry scheduling if configured. No persistent suggestion is created and no manual review interface is provided. All recovery decisions are visible via structured logs (`component=recovery`).

---

### 3.6 Retry Failed Imports

Re-queues imports that failed due to transient conditions.

**Retryable error detection:** Error messages are matched from queue item `errorMessage` and `StatusMessages`, plus history `Data` fields where applicable. Patterns are matched as case-insensitive regex.

**Retryable failure signatures:**

| Pattern | Reason |
|---|---|
| `(?i)permission denied` | NAS/FS permission |
| `(?i)access denied` | NAS/FS access |
| `(?i)no such file` | NAS/SMB temporarily unavailable |
| `(?i)connection refused` | Service unavailable |
| `(?i)connection timed out` | Network issue |
| `(?i)no space left` | Disk full |
| `(?i)input/output error` | Transient I/O |
| `(?i)file.*in use` | File locked |
| `(?i)destination.*locked` | Destination locked |
| `(?i)mount.*not available` | Mount point missing |
| `(?i)path.*not accessible` | Path not accessible |

**Non-retryable failures (always skipped):**

- Corrupted file / checksum mismatch
- Missing expected tracks / streams
- Resolution or codec mismatch
- Custom format score below cutoff (handled by §3.3)

**Retry schedule (configurable via `retryImports.retryIntervals`):**

Default schedule: 5 min, 15 min, 30 min, 1 h, 2 h, 4 h (6 retries over ~8 hours).

Each retry:
1. Re-checks that the item is still in the queue.
2. Re-runs Sonarr's manual-import preview for the download; a preview error
   (files not on disk, transient API failure) defers the retry.
3. Re-attempts the manual import with Sonarr's own quality, languages, and
   episode IDs, proving it via the queue poll (SPEC §3.2).
4. After all retries exhausted, marks permanently failed and logs a `warn`-level `import.failed-all-retries` event (see §9).

**Persistence:** In-memory only. If the agent restarts, pending retries are lost.

---

### 3.7 Safety

The central gatekeeper for every destructive action. No action bypasses it. See §7 for the full safety model.

Every detected issue is checked against config-derived automation settings and the always-enforced global constraints (no duplicate actions, cooldown, Sonarr connectivity, exclusion lists, state eligibility, TVDB gate). If all checks pass, the action is approved. In dry-run mode the approval produces a "would have" log entry instead of an API call.

**Conflicting detectors:** When multiple detectors flag the same queue item, only the most conservative action is taken. The priority order (most conservative first):

1. `log_only` (no mutation)
2. `remove_queue` (stuck download, not custom format, torrent client error)
3. `retry` (retry import)
4. `manual_import` (import recovery)

If two detectors propose the same action type, the one with the later `DetectedAt` timestamp is used. For each queue item, the queue monitor collects issues from both the built-in analysis (`buildIssue`) and all registered detectors, deduplicates by composite key (`seriesId:episodeId:downloadId`), and selects the highest-priority issue per poll cycle.

**Residual file cleanup (samples, NFOs, empty folders, `_unpack` dirs) is explicitly out of scope.** The agent only acts through Sonarr's API; it never deletes files itself and never reads the media filesystem.

---

### 3.8 Dry Run Mode

Global flag (`dryRun: true`) that disables all mutating API calls.

**Behavior when enabled:**

- Monitors run normally, including Sonarr API reads (previews, queue polls).
- Safety checks evaluate normally.
- Every approved action is logged as `action.recommended` with the full decision record and the message phrased as "Would have ...".
- No `POST`/`DELETE` requests are sent to Sonarr.
- Log entries are tagged `"dry_run": true`.

**Purpose:** Deploy the agent, observe behavior, tune settings, build confidence before enabling automation. A practical runbook for collecting and interpreting the dry-run text logs is in [DRYRUN_VALIDATION.md](DRYRUN_VALIDATION.md).

---

### 3.9 Torrent Client Error Removal

Downloads whose torrent client — qBittorrent itself or a qBit-compatible
bridge such as torboxarr (which presents TorBox as a qBittorrent API to
Sonarr) — reports a download error.

**Detection signature (verified against live Sonarr v4):** qBit `state=error`
is mapped by Sonarr v4 to queue `status="warning"` with
`trackedDownloadStatus="warning"` and the localized
`errorMessage` "qBittorrent is reporting an error" (or a matching status
message). The item never leaves the queue on its own: Sonarr's failed-download
handling only trips on `status=failed`, which qBit-bridge clients never
report, and the item's synthetic client hash never matches the grabbed
history, so even manual failure handling silently no-ops.

**Trigger conditions:**

| Condition | Detection |
|---|---|
| Tracked status | `trackedDownloadStatus` = `warning` |
| Error text | `errorMessage` or status messages match the configured pattern (default `(?i)qBittorrent is reporting an error`) |
| Age | >= `removeTorrentErrors.waitHours` (default 1 h) |

The signature is owned by this rule: the stuck-download detector defers to it
so the item is never double-handled.

**Actions (in order):**

1. Remove from queue (`DELETE /api/v3/queue/{id}?removeFromClient=true`).
2. **Blocklist** (`blocklistRelease`, default true): locate the grabbed
   history row by series/episode plus release title (the queue's downloadId
   is a synthetic hash that never matches history, so it is not used) and
   `POST /api/v3/history/failed/{historyId}` — the only working blocklist
   path for these clients, because the queue DELETE `blocklist` parameter
   silently no-ops when the hashes differ.
3. **Redownload** (`redownload`, default true): the `history/failed` command
   already triggers Sonarr's built-in redownload (`EpisodeSearchCommand`)
   when `AutoRedownloadFailed` is enabled (the default). When nothing was
   blocklisted (no history match, or blocklisting off), an explicit
   `EpisodeSearch` command is issued so a different release is grabbed
   instead of re-grabbing the same dead file.

**Safety checks (see §7):**

| Check | Expected |
|---|---|
| `rule.enabled` | `true` |
| `queue.trackedDownloadStatus` | `warning` |
| `error_message` | set |
| `age_hours` | `>= waitHours` (1) |
| `queue.trackedDownloadState` | `!= importing` |
| `retry.scheduled` | `false` |

The rule exists because plain removal alone re-grabs the same failed release
(torboxarr even dedupes submissions by fingerprint, so the new grab can
vanish into the dying job), creating an infinite loop.

---

### 3.10 Unknown-Series Download Resolution

Queue items whose series Sonarr does not know: `seriesId` and `episodeId`
are null, the download is otherwise complete, and the import is typically
blocked with the "Series title mismatch" status message. This happens when a
torrent bridge (torboxarr) reports a synthetic hash as the queue title, so
Sonarr's automatic import cannot match the release. The item would otherwise
be a permanent orphan — and it is invisible to queue fetches that omit
`includeUnknownSeriesItems=true` (SPEC §3.1, §12).

**Key insight:** the manual-import preview anchored to the tracked download
(`GET /api/v3/manualimport?downloadId=…`) resolves the real series and
episodes from the download folder and the grab history even though the queue
item carries no series identity (verified against prod: hash-titled
torboxarr downloads preview with their true series/episode, quality, and
languages). The resolution therefore does **more work than removal**:

1. **Preview** the download (read-only; also performed in dry-run so the
   recommendation names the exact outcome).
2. **Honor Sonarr's input about the file**: the preview reports the matched
   file's `rejections` (e.g. "Not an upgrade for existing episode file(s)",
   custom-format score below the cutoff). A rejected file is **not**
   force-imported — forcing the ManualImport command would override the
   episode's quality/custom-format decisions and could downgrade an episode
   that already has a better file. When several files are matched, a
   rejection-free one is preferred.
3. **Import** when a rejection-free preview file has matched episodes: a
   `ManualImport` command is submitted with Sonarr's own quality, languages,
   and episode IDs plus the series ID of the matched episode (fetched from
   Sonarr), and the import is proven by the queue poll (SPEC §3.2). No
   custom-format upgrade gate applies — this is exactly the UI's manual
   Import button.
4. **Fallback removal** when the preview finds no series match, fails, or the
   matched file carries an import rejection: the item is removed from the
   queue (`DELETE /api/v3/queue/{id}`) and the blocker's reasons are logged.
   Blocklisting is not attempted: with no series identity, no grabbed
   history can be located.

**Safety checks (see §7):**

| Check | Expected |
|---|---|
| `rule.enabled` | `true` |
| `queue.status` | `completed\|warning\|failed` |
| `series.unknown` | `seriesId=0 episodeId=0` |
| `age_hours` | `>= waitHours` (1) |
| `queue.trackedDownloadState` | `!= importing` |
| `retry.scheduled` | `false` |

Unknown-series items are never grouped into episode reconciliation — they
have no episode to reconcile — and the stuck-download detector defers to this
rule while it is enabled.

---

### 3.11 Action Log

The agent's only output surface. Every action produces exactly one structured log line:

| Event | Level | When |
|---|---|---|
| `action.taken` | `info` | Action executed (dry-run off) |
| `action.recommended` | `info` | Action approved but dry-run on — "Would have ..." |
| `action.skipped` | `info` | Action rejected by a safety check, with reason. An identical rejection for the same item, action, and reason is logged at `debug` for 5 minutes after the previous `info` line — stuck items would otherwise spam one full line per poll |
| `import.failed-all-retries` | `warn` | All retries exhausted; manual intervention required |
| `error.sonarr-unreachable` | `error` | Sonarr connectivity lost; monitors pause with backoff |
| `error.sonarr-auth` | `error` | Sonarr rejected the credentials (401/403); monitors pause until they are fixed |

Successful detections that produce no action, and confidence breakdowns below threshold, are logged as routine `info` entries (`component=recovery`, `component=queue_monitor`). The exact field schema is defined in §9.

---

## 4. Architecture

### High-Level Component Diagram

```
                         +---------------------+
                         |      Sonarr         |
                         |  (external service) |
                         +----------+----------+
                                    |
                                REST API
                                    |
  +---------------------------------+-----------------------------------+
  |                        Sonarr Recovery Agent                        |
  |                                                                     |
  |  +------------------+  +------------------+                        |
  |  | Queue Monitor    |  | Health Monitor   |                        |
  |  | (30s interval)   |  | (60s interval)   |                        |
  |  +--------+---------+  +--------+---------+                        |
  |           |                      |                                 |
  |           +----------------------+                                 |
  |                        +---------v---------+                       |
  |                        |   Issue Detector  |                       |
  |                        +---------+---------+                       |
  |           +----------------------+----------------------+          |
  |           |                      |                      |          |
  |  +--------v---------+  +--------v---------+  +--------v---------+  |
  |  | Stuck Download   |  | Not Custom Fmt   |  | Import Recovery  |  |
  |  | Detector         |  | Upgrade Detector |  | Detector         |  |
  |  +--------+---------+  +--------+---------+  +--------+---------+  |
  |           |                      |                      |          |
  |           +----------------------+----------------------+          |
  |                                  |                                 |
  |                        +---------v---------+                       |
  |                        |   Safety Engine   |                       |
  |                        |  (gates + globals)|                       |
  |                        +---------+---------+                       |
  |                                  |                                 |
  |              +-------------------+-------------------+             |
  |              |                   |                   |             |
  |  +-----------v---------+  +------v-------+  +--------v--------+    |
  |  | Action Executor     |  | Retry Sched. |  | Decision Logger |    |
  |  +---------------------+  +--------------+  +-----------------+    |
  |                                                                     |
  |  +----------------+  +----------------+                            |
  |  | Config Loader  |  | Structured Log |                            |
  |  +----------------+  +----------------+                            |
  +--------------------------------------------------------------------+
```

### Internal Component Communication

- **Monitors** poll on internal tickers (no shared scheduler package).
- **Queue Monitor** produces `Issue` values via the detector pipeline and deduplicates by composite key.
- **Safety Engine** receives `Issue` values, evaluates config-derived gates + global constraints, and produces `Decision` values.
- **Action Executor** receives approved `Decision` values and performs Sonarr API calls (or logs "would have" in dry-run).
- **Retry Scheduler** manages an in-memory timer-based retry queue (`executor/retry.go`).
- **Decision Logger** emits every decision as a structured key=value text log line (§9).

### Directory Structure

```
sonarr-remediator/
├── cmd/
│   └── sonarr-remediator/
│       └── main.go              # Entry point, config loading, service wiring, graceful shutdown
├── internal/
│   ├── config/
│   │   ├── config.go            # Configuration struct & loading (YAML + env)
│   │   ├── defaults.go          # Default values
│   │   └── validate.go          # Startup config validation (strict)
│   ├── sonarr/
│   │   ├── client.go            # Sonarr REST API client (version detection, rate limiting)
│   │   ├── queue.go             # Queue endpoint calls
│   │   ├── history.go           # History endpoint calls
│   │   ├── manual_import.go     # Manual import calls
│   │   ├── system.go            # System status, health, version
│   │   ├── quality.go           # Quality definitions (fetched at startup)
│   │   ├── language.go          # Language definitions (fetched at startup)
│   │   └── extras.go            # Episode/file/series helpers
│   ├── monitors/
│   │   ├── queue_monitor.go     # Queue polling, issue dedup by priority, same-episode gate
│   │   └── health_monitor.go    # Sonarr connectivity
│   ├── detectors/
│   │   ├── detector.go          # Detector interface, Issue types
│   │   ├── stuck_download.go    # Stuck download detection
│   │   ├── not_custom_format.go # "Not custom format upgrade" detection
│   │   └── import_recovery.go   # Failed import recovery detection
│   ├── recovery/
│   │   └── import.go            # Preview-based import recovery, reconcile import, import proving
│   ├── safety/
│   │   ├── engine.go            # Safety gates + global constraints + decision log
│   │   └── engine_test.go
│   ├── executor/
│   │   ├── executor.go          # Action execution (remove queue, manual import)
│   │   └── retry.go             # Retry scheduling & execution
│   ├── logging/
│   │   └── logging.go           # Structured logger (slog)
│   └── types/
│       └── types.go             # Domain types (Issue, Decision, QueueItem, ...)
├── config.example.yaml
├── Dockerfile
├── docker-compose.example.yaml
├── go.mod
├── go.sum
├── Makefile
├── README.md
├── SPEC.md
└── DOCKER_SPEC.md
```

---

## 5. Component Specifications

### 5.1 Sonarr API Client (`internal/sonarr/client.go`)

```go
type Client struct {
    BaseURL    *url.URL
    APIKey     string
    HTTPClient *http.Client
    Version    string   // detected Sonarr version, e.g. "4.0.0.741"
}
```

**Client methods:**

```go
func (c *Client) GetQueue(ctx context.Context) ([]QueueItem, error)
func (c *Client) GetHistory(ctx context.Context, params HistoryParams) ([]HistoryItem, error)
func (c *Client) GetSystemStatus(ctx context.Context) (SystemStatus, error)
func (c *Client) RemoveQueueItem(ctx context.Context, id int, blocklist bool) error
func (c *Client) Parse(ctx context.Context, path string) (*ParseResult, error)
func (c *Client) ManualImportPreview(ctx context.Context, downloadID string) ([]ManualImportFile, error)
func (c *Client) ManualImportCommand(ctx context.Context, cmd ManualImportCommand) error
func (c *Client) GetQualityDefinitions(ctx context.Context) ([]QualityDefinition, error)
func (c *Client) GetLanguages(ctx context.Context) ([]Language, error)
func (c *Client) GetEpisode(ctx context.Context, episodeID int) (*EpisodeResource, error)
func (c *Client) GetEpisodeFile(ctx context.Context, episodeFileID int) (*EpisodeFileResource, error)
func (c *Client) GetDownloadClients(ctx context.Context) ([]DownloadClientResource, error)
func (c *Client) GetSeries(ctx context.Context, seriesID int) (*SeriesResource, error)
```

**Convenience methods:**

```go
// GetEpisodeFileForEpisode fetches the episode, checks hasFile,
// and if true, fetches the episode file. Returns nil if no file exists.
func (c *Client) GetEpisodeFileForEpisode(ctx context.Context, episodeID int) (*EpisodeFileResource, error)
```

**Version detection:**
At startup, `GetSystemStatus` returns a `Version` string like `"4.0.0.741"`. The major version (first number) determines blocklist parameter naming and event type handling.

**Error handling:**
- All calls use `context.Context` for cancellation and timeouts.
- `401`/`403` responses trigger an `error.sonarr-auth` log entry; monitors continue disabled until credentials are fixed.
- Other `4xx` (except `429`) are terminal errors for that item.
- `5xx` and network errors trigger exponential backoff with jitter (max 3 retries).
- Concurrent request limiting via semaphore (max 5 concurrent requests).
- If Sonarr becomes unreachable, monitors pause with exponential backoff (1 min, 2 min, 4 min, ..., max 10 min) and log `error.sonarr-unreachable`. Once connectivity resumes, perform a full state refresh.

---

### 5.2 Queue Monitor (`internal/monitors/queue_monitor.go`)

```go
type QueueMonitor struct {
    client     *sonarr.Client
    interval   time.Duration
    issues     chan<- Issue
    detectors  []Detector
    getHistory func(episodeID int) []HistoryItem
    engine     *safety.Engine
    cfg        *config.Config
}
```

Each tick: fetch the queue via `GetQueue()`, run the built-in issue analysis
plus all registered detectors over every item, deduplicate by composite key
selecting the highest-priority issue, and emit the winning issue per item for
eligible states. Every poll is a full evaluation: detection is stateless per
item (repeated issues are suppressed by the safety engine's duplicate-action
and cooldown constraints), so no cross-poll diff state is kept. Items with an
approved decision in the last 5 minutes are not re-evaluated at all (SPEC §7
constraint 1), so stuck items do not re-log detection and rejection lines
every poll. The same-episode gate for not-custom-format removals (SPEC §3.3)
is enforced on the winning candidate, covering both detection methods.
Detectors are injected at construction via `NewQueueMonitor(client, cfg,
engine, issues, detectors, logger)`.

---

### 5.3 Issue Detector Interface (`internal/detectors/detector.go`)

```go
type Detector interface {
    Name() string
    Detect(ctx context.Context, item QueueItem, history []HistoryItem, client *sonarr.Client) (*Issue, error)
}

type Issue struct {
    ID             string         `json:"id"`
    Type           IssueType      `json:"type"`
    Severity       Severity       `json:"severity"`
    QueueItem      QueueItem      `json:"queueItem"`
    RelatedHistory []HistoryItem  `json:"relatedHistory,omitempty"`
    Details        map[string]any `json:"details"`
    DetectedAt     time.Time      `json:"detectedAt"`
}

type IssueType string

const (
    IssueStuckDownload   IssueType = "stuck_download"
    IssueNotCustomFormat IssueType = "not_custom_format_upgrade"
    IssueImportFailed    IssueType = "import_failed"
)

type Severity string

const (
    SeverityInfo     Severity = "info"
    SeverityWarning  Severity = "warning"
    SeverityCritical Severity = "critical"
)
```

---

### 5.4 Safety Engine (`internal/safety/engine.go`)

```go
type Engine struct {
    config      *Config            // automation settings (gates)
    activeItems map[string]time.Time  // active actions (composite key)
    lastAction  map[string]time.Time  // cooldown ("seriesId:episodeId")
}

func (e *Engine) Evaluate(ctx context.Context, issue Issue) (*Decision, error)

type Decision struct {
    Issue     Issue            `json:"issue"`
    Action    ActionType       `json:"action"`
    Checks    []CheckResult    `json:"checks"`
    Approved  bool             `json:"approved"`
    Reason    string           `json:"reason,omitempty"`
    Timestamp time.Time        `json:"timestamp"`
    DryRun    bool             `json:"dryRun"`
}

type CheckResult struct {
    Check    string `json:"check"`
    Expected string `json:"expected"`
    Actual   string `json:"actual"`
    Passed   bool   `json:"passed"`
}
```

The engine evaluates the config-derived gates for the issue's automation rule, then the always-enforced global constraints (§7). Every check is recorded in `Decision.Checks` with its actual value so dry-run logs show exactly why an action passed or failed.

---

### 5.5 Action Executor (`internal/executor/executor.go`)

```go
type Executor struct {
    sonarrClient *sonarr.Client
    dryRun       bool
}

func (e *Executor) Execute(ctx context.Context, decision Decision) error
```

Each handler:
1. Checks `dryRun` — if true, logs `action.recommended` ("Would have ...") and returns nil.
2. Performs the Sonarr API call(s).
3. Logs `action.taken` with the decision record.

Supported action types: `remove_queue` (with optional blocklist), `manual_import`, `retry` (delegated to the retry scheduler), `log_only`.

---

## 6. Data Models

### Core Domain Types

```go
// ─── Queue ───────────────────────────────────────────────────────────

// Page is Sonarr's paged-list envelope (GET /api/v3/queue, /api/v3/history):
// records are nested under "records" rather than served as a bare array.
type Page[T any] struct {
    Page         int `json:"page"`
    PageSize     int `json:"pageSize"`
    TotalRecords int `json:"totalRecords"`
    Records      []T `json:"records"`
}

type QueueItem struct {
    ID                    int             `json:"id"`
    SeriesID              int             `json:"seriesId"`
    EpisodeID             int             `json:"episodeId"`
    SeriesTitle           string          `json:"seriesTitle"` // not present on Sonarr v4 queue payloads; kept for tests and non-v4 compatibility
    EpisodeTitle          string          `json:"episodeTitle"`
    Title                 string          `json:"title"` // release title (Sonarr v4)
    Quality               QualityModel    `json:"quality"`
    CustomFormats         []CustomFormat  `json:"customFormats"`
    CustomFormatScore     int             `json:"customFormatScore"`
    Size                  int64           `json:"size"`
    Status                string          `json:"status"`                // queued|paused|downloading|completed|warning|failed
    TrackedDownloadStatus string          `json:"trackedDownloadStatus"` // ok|warning|error
    TrackedDownloadState  string          `json:"trackedDownloadState"`  // downloading|importPending|importing|imported|importFailed|downloadFailed
    StatusMessages        []StatusMessage `json:"statusMessages"`
    ErrorMessage          string          `json:"errorMessage"`
    DownloadID            string          `json:"downloadId"`
    OutputPath            string          `json:"outputPath"`
    Added                 time.Time       `json:"added"`
}

type CustomFormat struct {
    ID   int    `json:"id"`
    Name string `json:"name"`
}

type StatusMessage struct {
    Title    string   `json:"title"`
    Messages []string `json:"messages"`
}

// ─── History ─────────────────────────────────────────────────────────

type HistoryItem struct {
    ID          int               `json:"id"`
    SeriesID    int               `json:"seriesId"`
    EpisodeID   int               `json:"episodeId"`
    SourceTitle string            `json:"sourceTitle"`
    EventType   string            `json:"eventType"`  // "grabbed"|"downloadFolderImported"|"downloadFailedImport"|"downloadIgnored"|"episodeFileDeleted"
    Quality     QualityModel      `json:"quality"`
    Date        time.Time         `json:"date"`
    Data        map[string]string `json:"data"`
}

type HistoryParams struct {
    Page          int    `json:"page"`
    PageSize      int    `json:"pageSize"`
    SortKey       string `json:"sortKey"`
    SortDirection string `json:"sortDirection"`
    EventType     int    `json:"eventType,omitempty"`  // Sonarr API uses int for event type filter (1=grabbed, 3=imported, 4=failedImport, 7=ignored)
    SeriesID      int    `json:"seriesId,omitempty"`
    EpisodeID     int    `json:"episodeId,omitempty"`
}
// Note: HistoryParams.EventType is int because Sonarr's API query param expects an integer.
// HistoryItem.EventType is string because that's what the API response returns.

// ─── Manual Import ───────────────────────────────────────────────────

type ManualImportCommand struct {
    Name       string                    `json:"name"`
    ImportMode string                    `json:"importMode"`
    Files      []ManualImportCommandFile `json:"files"`
}

// ManualImportCommandFile is one file inside a ManualImportCommand. Episodes
// are plural and carry the season implicitly; seasonNumber is not sent.
type ManualImportCommandFile struct {
    Path       string         `json:"path"`
    SeriesID   int            `json:"seriesId"`
    EpisodeIDs []int          `json:"episodeIds"`
    Quality    QualityModel   `json:"quality"`
    Languages  []LanguageModel `json:"languages"`
    DownloadID string         `json:"downloadId"`
}

type QualityModel struct {
    Quality  Quality  `json:"quality"`
    Revision Revision `json:"revision"`
}

type Quality struct {
    ID   int    `json:"id"`
    Name string `json:"name"`
}

type Revision struct {
    Version int `json:"version"`
}

type LanguageModel struct {
    ID   int    `json:"id"`
    Name string `json:"name"`
}

// ─── Parse ───────────────────────────────────────────────────────────

type ParseResult struct {
    Title             string             `json:"title"`
    ParsedEpisodeInfo *ParsedEpisodeInfo `json:"parsedEpisodeInfo"`
    Series            *SeriesInfo        `json:"series"`
    Episodes          []EpisodeLookup    `json:"episodes"`
}

type ParsedEpisodeInfo struct {
    ReleaseTitle           string        `json:"releaseTitle"`
    SeriesTitle            string        `json:"seriesTitle"`
    SeasonNumber           int           `json:"seasonNumber"`
    EpisodeNumbers         []int         `json:"episodeNumbers"`
    AbsoluteEpisodeNumbers []int         `json:"absoluteEpisodeNumbers"`
    FullSeason             bool          `json:"fullSeason"`
    Quality                QualityModel  `json:"quality"`
    Language               LanguageModel `json:"language"`
}

type SeriesInfo struct {
    Title  string `json:"title"`
    TVDBID int    `json:"tvdbId"`
    ImdbID string `json:"imdbId"`
}

type EpisodeLookup struct {
    ID            int    `json:"id"`
    EpisodeNumber int    `json:"episodeNumber"`
    SeasonNumber  int    `json:"seasonNumber"`
    Title         string `json:"title"`
}

// ─── Episode / File ──────────────────────────────────────────────────

type EpisodeResource struct {
    ID            int    `json:"id"`
    SeriesID      int    `json:"seriesId"`
    SeasonNumber  int    `json:"seasonNumber"`
    EpisodeNumber int    `json:"episodeNumber"`
    Title         string `json:"title"`
    HasFile       bool   `json:"hasFile"`
    EpisodeFileID int    `json:"episodeFileId"`
}

type EpisodeFileResource struct {
    ID                  int         `json:"id"`
    SeriesID            int         `json:"seriesId"`
    SeasonNumber        int         `json:"seasonNumber"`
    EpisodeNumber       int         `json:"episodeNumber"`
    RelativePath        string      `json:"relativePath"`
    Quality             QualityName `json:"quality"`
    QualityCutoffNotMet bool        `json:"qualityCutoffNotMet"`
    Size                int64       `json:"size"`
}
// QualityName accepts either a plain quality name string or Sonarr's quality
// object ({quality:{name:...},revision:{...}}), normalizing to the name.
// Note: Sonarr's episode file API response does not include a quality ID directly.
// Quality comparison in the pre-import check maps the existing file's quality name
// to a QualityDefinition (fetched at startup) to obtain a weight; if the lookup
// fails, comparison falls back to quality name ordering.

// ─── Series ──────────────────────────────────────────────────────────

type SeriesResource struct {
    ID     int    `json:"id"`
    Title  string `json:"title"`
    TVDBID int    `json:"tvdbId"`
    Path   string `json:"path"`
}

// ─── Definitions (Fetched at Startup) ────────────────────────────────

type QualityDefinition struct {
    ID     int    `json:"id"`
    Name   string `json:"name"`
    Title  string `json:"title"`
    Weight int    `json:"weight"`  // Sonarr's internal ranking; higher = better
}

type Language struct {
    ID   int    `json:"id"`
    Name string `json:"name"`
}

// ─── Download Client (for Root Folder Discovery) ─────────────────────

type DownloadClientResource struct {
    ID     int                   `json:"id"`
    Name   string                `json:"name"`
    Fields []DownloadClientField `json:"fields"`
}

type DownloadClientField struct {
    Name  string     `json:"name"`
    Value FlexString `json:"value"`
}
// FlexString accepts string, number, boolean, or null JSON values: Sonarr
// download client fields are heterogeneous (e.g. a port is a number), while
// the root-path fields the agent reads are strings.
// Root folder extraction: iterate Fields, look for Name == "downloadFolder"
// or Name == "tvDownloadFolder". The Value is the download root path (string).

// ─── System ──────────────────────────────────────────────────────────

type SystemStatus struct {
    Version string `json:"version"`  // e.g. "4.0.0.741"
}
```

### Action Types

```go
type ActionType string

const (
    ActionLogOnly       ActionType = "log_only"
    ActionRemoveQueue   ActionType = "remove_queue"
    ActionRetry         ActionType = "retry"
    ActionManualImport  ActionType = "manual_import"
    ActionReconcile     ActionType = "reconcile"
)
```

### Reconcile Plan

```go
type ReconcilePlan struct {
    SeriesID  int          `json:"seriesId"`
    EpisodeID int          `json:"episodeId"`
    Winner    QueueItem    `json:"winner"`
    Discards  []QueueItem  `json:"discards"`
}

func (p ReconcilePlan) EpisodeKey() string // "seriesId:episodeId"
```

The plan travels from the queue monitor to the executor inside
`Issue.Details["reconcile_plan"]`; the issue itself is type `reconcile` and
anchors on the winner. `selector.IsUpgrade(releaseScore, existingScore,
releaseWeight, existingWeight, weightsOK)` implements the upgrade decision.

---

## 7. Safety

### Config-Derived Gates

Each automation setting generates the checks that gate its action. All checks within a rule use AND logic (short-circuit on first failure).

**Example** — `automation.removeNotCustomFormat` generates:

| Check | Expected | Where enforced |
|---|---|---|
| `rule.enabled` | `true` | engine |
| `queue.status` | `completed` | engine |
| `queue.trackedDownloadState` | `!= importing` | engine |
| `status_message` | matches `(?i)not.*(custom format|an upgrade)` | detector (Method A) |
| `age_hours` | `>= 2` (waitHours) | engine |
| `retry.scheduled` | `false` | engine |
| `other_queue_item_for_episode` | `false` | queue monitor (winning candidate, both methods) |

The check tables in §3.2 and §3.3 enumerate these gates for each feature. The engine records the actual value of every check it evaluates in the decision log, so dry-run output explains each pass/fail; checks enforced outside the engine (status-message and same-episode matching) are part of detection and reported in the issue details.

**`automation.reconcile` gates the `reconcile` issue (one per episode plan, §3.2):**

| Check | Expected |
|---|---|
| `rule.enabled` | `true` (automation.reconcile.enabled) |
| `queue.status` | `completed\|warning\|failed` |
| `queue.trackedDownloadState` | `!= importing` |
| `retry.scheduled` | `false` |

Approval covers the whole plan: the winner's import-or-removal and every
discard removal. The decision log adds `episode_key`, `upgrade` (winner's
upgrade decision), and `discards` (id/release/score list).

### Global Constraints (Always Enforced)

1. **No duplicate actions**: Same item, same action, within 5 minutes → skip.
2. **Cooldown period**: At least 30 minutes between actions on same series/episode pair (key: `seriesId:episodeId`). Unknown-series items (both IDs zero) use their download ID as the bucket, so distinct stuck downloads are not serialized on one `0:0` key.
3. **Sonarr connectivity required**: Last health check must have succeeded.
4. **Atomicity**: If first step of multi-step action fails, do not proceed.
5. **Exclusion list**: Any item whose series ID matches `exclusions.seriesIds` or whose root path has a prefix matching any `exclusions.rootPaths` is skipped. Root path matching uses prefix comparison on the item's download path.
6. **State eligibility**: Only items with status in `completed`, `warning`, or `failed` are evaluated.
7. **TVDB ID gate**: In import recovery, if the parsed TVDB ID does not match the expected series TVDB ID, confidence is 0 and the item is skipped. This is enforced before confidence scoring.

### Decision Log Format

Every evaluation produces a key=value text log line; when an action is
approved or rejected, the decision is logged with the event name as the
`type` token:

```text
time=2026-07-23T10:12:00.000Z level=INFO type=action.recommended msg="Would have removed queue item 420" component=safety decision_id=dec_abc123 item= key="42:105:abc123" id=420 title="Ubuntu.S01E05.1080p.WEB-DL" series=Ubuntu episode=S01E05 trigger=not_custom_format_upgrade checks=[{"check":"queue.status","expected":"completed","actual":"completed","passed":true},{"check":"age_hours","expected":">= 2","actual":"6.3","passed":true}] action=remove_queue message="Would have removed queue item 420" dry_run=true
```

---

## 8. Configuration Schema

```yaml
# Sonarr Recovery Agent Configuration

# ─── Sonarr Connection ───
sonarr:
  url: http://sonarr:8989
  apiKey: ""
  timeout: 30s
  maxConcurrency: 5

# ─── Monitoring ───
monitoring:
  queueInterval: 30s
  healthInterval: 60s
  startupDelay: 10s

# ─── File System ───
paths:
  downloadRoots: []
  # Legacy: recovery is filesystem-independent (SPEC §3.4) and these keys are
  # parsed and validated for compatibility only; they no longer affect behavior.
  agentRoot: ""     # The root path as seen by the agent (e.g., /data)
  sonarrRoot: ""    # The root path as seen by Sonarr (e.g., /data)

# ─── Exclusion ───
exclusions:
  seriesIds: []     # Series IDs to never touch.
  rootPaths: []     # Root folder prefixes to exclude. Any queue item whose
                    # download path starts with one of these is skipped.

# ─── Automation ───
automation:

  removeNotCustomFormat:
    enabled: true
    waitHours: 2
    blocklistRelease: false
    statusMessageRegex: ""

  removeBrokenDownloads:
    enabled: true
    waitHours: 6
    blocklistRelease: false
    errorConditions:
      - missing_files
      - abandoned

  removeTorrentErrors:
    enabled: true
    waitHours: 1
    errorMessagePattern: "" # default: "(?i)qBittorrent is reporting an error"
    blocklistRelease: true
    redownload: true

  resolveUnknownSeries:
    enabled: true
    waitHours: 1

  retryImports:
    enabled: true
    retryIntervals:
      - 5m
      - 15m
      - 30m
      - 1h
      - 2h
      - 4h
    retryableErrors:
      - "(?i)permission denied"
      - "(?i)access denied"
      - "(?i)no such file"
      - "(?i)connection refused"
      - "(?i)connection timed out"
      - "(?i)no space left"
      - "(?i)input/output error"
      - "(?i)file.*in use"
      - "(?i)destination.*locked"
      - "(?i)mount.*not available"
      - "(?i)path.*not accessible"

  autoManualImport:
    enabled: false

  # Episode reconciliation: map targeted hits by episode, import the highest
  # custom-format-score release as an upgrade, remove the rest with
  # removeFromClient=true (SPEC §3.2).
  reconcile:
    enabled: true
    minimumConfidence: 95

# ─── Logging ───
logging:
  level: info

# ─── Global ───
dryRun: true
```

### Path Translation

Legacy: the recovery and retry pipelines are filesystem-independent and never
translate paths — Sonarr's manual-import preview returns paths in Sonarr's
own view, submitted unchanged (SPEC §1 "Path Translation"). The
`paths.agentRoot`/`paths.sonarrRoot` keys are parsed and validated for
compatibility only.

### Environment Variable Overrides

Prefix `SRA_` with double-underscore separators:

```
SRA_SONARR__URL=http://sonarr:8989
SRA_SONARR__API_KEY=abc123
SRA_DRY_RUN=false
SRA_LOGGING__LEVEL=debug
SRA_AUTOMATION__REMOVE_NOT_CUSTOM_FORMAT__ENABLED=true
SRA_AUTOMATION__AUTO_MANUAL_IMPORT__MINIMUM_CONFIDENCE=90
```

### Startup Validation (Strict)

The agent must fail fast with clear errors for:

| Check | Rule |
|---|---|
| `sonarr.url` | Must be a valid HTTP(S) URL |
| `sonarr.apiKey` | Must be non-empty |
| `sonarr.timeout` | Must be > 0 |
| All duration values | Must parse as valid Go durations |
| `autoManualImport.minimumConfidence` | Must be 0-100 |
| `retryImports.retryIntervals` | Must be non-empty if `retryImports.enabled` |
| `paths.downloadRoots` | If provided, each path must exist and be readable |
| `paths.agentRoot` and `paths.sonarrRoot` | If one is set, both must be set; both must exist |
| `exclusions.seriesIds` | Each ID must be a positive integer |
| `exclusions.rootPaths` | If provided, each path must exist |
| Config file | Must parse as valid YAML with no unknown top-level keys |

---

## 9. Logging & Action Log

### Logging

Structured key=value text logs to stderr (container-native; consumed via `docker logs`, DOCKER_SPEC.md §1), implemented with the standard library `log/slog`.

- Levels: `debug`, `info`, `warn`, `error`.
- Line shape: `time=... level=... type=... msg=...` followed by the remaining fields as `key=value` tokens. Values with spaces are quoted.
- `type` is the filterable token: the `event` name when present (e.g. `action.taken`, `error.sonarr-auth`), otherwise the `component`, otherwise `log`.
- `component` values: `config`, `sonarr`, `queue_monitor`, `health_monitor`, `detector`, `recovery`, `safety`, `executor`, `retry`, `main`.

### Action Log Events

| Event | Level | `dry_run` | Message shape |
|---|---|---|---|
| `action.taken` | `info` | `false` | "Removed queue item 420" |
| `action.recommended` | `info` | `true` | "Would have removed queue item 420" |
| `action.skipped` | `info` | — | "Skipped queue item 420: cooldown active" |
| `reconcile.plan` | `info` | — | "episode reconciliation: import highest-scoring release, discard rest" (episode_key, winner, discards) |
| `import.failed-all-retries` | `warn` | — | "Import permanently failed after 6 retries — manual intervention required" |
| `error.sonarr-unreachable` | `error` | — | "Sonarr at http://sonarr:8989 not responding; monitors paused" |
| `error.sonarr-auth` | `error` | — | "Sonarr rejected the configured credentials; monitors paused" |

**Event-specific fields** (the `event` renders as the `type` token):

- Action events: `type`, `decision_id`, `item` (key, id, title, series, episode), `trigger`, `checks` (see §7), `action`, `reason` (for `action.skipped`). Reconcile action events additionally carry `episode_key`, `upgrade`, and `discards` (id/release/score list).
- Recovery events: `type`, `item`, `confidence`, `confidence_breakdown` (tvdb, season, episode, quality, language), `candidate_path`.
- Retry events: `type`, `item`, `attempt`, `retries_left`, `next_retry_at`.

Every approved action produces exactly one action event line. Routine detections that produce no action are `info` entries under `component=queue_monitor`/`detector` and are not events.

---

## 10. Testing Strategy

### Unit Tests

| Layer | Coverage Target | Focus |
|---|---|---|
| `internal/safety/` | 95%+ | Gate evaluation, global constraints, decision log |
| `internal/recovery/` | 90%+ | Confidence scoring (Sonarr match), preview selection, import proving |
| `internal/config/` | 90%+ | Config loading, env overrides, validation |
| `internal/detectors/` | 85%+ | Issue detection with mocked Sonarr data |
| `internal/executor/` | 85%+ | Action dispatch, dry-run, error handling |
| `internal/logging/` | 80%+ | Event rendering, dry-run message shapes |

### Integration Tests

**Sonarr API Mock:** `httptest.Server` simulating Sonarr API responses for all endpoints.

**End-to-end scenarios:**
1. Stuck download detected → safety check passes → removal (or dry-run skip).
2. "Not a Custom Format Upgrade" detected via queue message → removal.
3. "Not a Custom Format Upgrade" detected via history event → removal.
4. Failed import → preview match → high confidence → auto-import.
5. Failed import → no Sonarr match → confidence 0 → skip.
6. Failed import → Sonarr match → medium confidence → log-only with breakdown.
7. Failed import → transient error → retry schedule → recovery on retry N.
8. Failed import → all retries exhausted → `import.failed-all-retries` log event emitted.
9. Multi-episode file → import via multiple ManualImport commands (one episode per command).
10. Pre-import check finds existing better file → import rejected.
11. Pre-import check: existing file has lower quality → import proceeds.
12. Pre-import check: episode has no file → import proceeds.
13. Dry-run enabled → all detections work, zero mutations, `action.recommended` entries emitted.
14. Exclusion list (seriesId) match → item skipped entirely.
15. Exclusion list (rootPath prefix) match → item skipped entirely.
16. Two same-episode targeted hits with `dryRun=false` → winner imported (ManualImport command via POST /api/v3/command, queue item cleared), discard removed (`DELETE /api/v3/queue/{id}?removeFromClient=true`), winner never deleted, `reconcile.plan` logged.
17. Failed-import recovery against a **v4** mock → the preview flow imports normally (v4 serves the manual-import preview; the removed parse pipeline is what 204'd on `path=`).
18. `POST /api/v3/manualimport` (reprocess) is evaluate-only → rejection verdict, tracked download stays in the queue, nothing imported.

### Test Fixtures

Real anonymized Sonarr API responses stored as JSON fixtures.

---

## 11. Deployment

Container packaging — image build, runtime contract, Compose composition,
healthcheck, image tags, and update/rollback — is specified in
[DOCKER_SPEC.md](DOCKER_SPEC.md). This section covers only the process
behavior that deployment relies on.

### Graceful Shutdown

On receiving `SIGTERM` or `SIGINT`:

1. Stop all monitors (no new poll cycles).
2. Allow in-progress manual imports and Sonarr API calls to complete (timeout: 30 s).
3. Flush decision log ring buffer to stderr.
4. Exit with code 0.

`stop_grace_period` must be at least 30 seconds (DOCKER_SPEC.md §8). A second
signal causes immediate exit.

---

## 12. Appendix: Sonarr API Surface

The agent uses the Sonarr v3 API (used by both Sonarr v3 and v4 installations). The agent detects the running version at startup and adapts where minor differences exist.

### History
- `GET /api/v3/history` — Event history (paged envelope; `records` is unwrapped). Params: `page`, `pageSize`, `sortKey`, `sortDirection`, `eventType` (int: 1=grabbed, 3=imported, 4=failedImport, 7=ignored), `episodeId`, `seriesId`.

### Manual Import
- `GET /api/v3/manualimport` — Preview the importable files of a tracked
  download. The agent mirrors the UI call exactly: `downloadId=<id>` only
  (Sonarr resolves the folder from the tracked download and anchors the
  series/episode match to the grab history — without it Sonarr answers
  "Unknown Series" even for files it can match) plus
  `filterExistingFiles=false` (the default filter drops files whose episode
  already has a media file). Response: one item per file with Sonarr's
  `quality`, `languages`, matched `episodes`, and `rejections`. Contract
  details verified against prod v4: an empty `downloadId` is HTTP 500 (the
  empty path throws); a `downloadId` Sonarr does not know returns `[]`; a
  known downloadId whose files are not on disk yet (still downloading)
  also 500s.
- `POST /api/v3/command` — Execute a manual import via the `ManualImport`
  command (the same command the UI's Import button sends). Body:
  `{"name": "ManualImport", "importMode": "auto", "files": [{path,
  seriesId, episodeIds, quality, languages, downloadId}]}`. Sonarr v4
  answers **201 Created** with the command resource and executes the import
  unconditionally (no custom-format upgrade gate), removing the tracked
  download on completion — success is proven by polling `GET /api/v3/queue`
  until the item's ID disappears (bounded by the poll timeout); a surviving
  item or non-2xx response means the import did not commit and the item is
  left in the queue.
- `POST /api/v3/manualimport` — **Evaluate-only** on live v4
  (`ManualImportController.ReprocessItems`): it decodes a JSON array of
  `{path, seriesId, seasonNumber, episodeIds, quality, languages,
  downloadId}` and returns one verdict item per file with `rejections`
  (e.g. "Not an upgrade for existing episode file(s)") — it **never
  imports**. The dev-test mock models this faithfully so a regression to
  this endpoint fails tests (verdict, no import, queue unchanged).

The failed-import recovery (§3.4) and import retries (§3.6) run entirely on
the manual-import preview + command flow above, which Sonarr v3 and v4 both
serve normally. The agent no longer calls the parse endpoint (`GET
/api/v3/parse`), which v4 answers with 204 No Content to `path=` calls
(verified against prod) and v3 answers normally — the historical
parse-based recovery pipeline is removed.

### Queue
- `GET /api/v3/queue` — Paged envelope (`{page, pageSize, totalRecords,
  records}`); the client requests `page=1&pageSize=1000` and unwraps
  `records`. The request sets `includeUnknownSeriesItems=true` explicitly:
  Sonarr's default hides items whose series is not in the library, which are
  exactly the import-blocked stuck items a remediator must see (verified
  against prod: series-title-mismatch items carry seriesId/episodeId null,
  status completed, trackedDownloadStatus warning, state importBlocked).
- `DELETE /api/v3/queue/{id}` — Remove item. Params: `blocklist=true` (v3)
  or `removeFromClient=true` (v4 fallback, version-detected). A successful
  delete removes the item; an unknown id is HTTP 404 `{"message":
  "NotFound"}` (verified against prod).

### System
- `GET /api/v3/system/status` — System status. Response includes `version` (string).

### Quality & Language
- `GET /api/v3/qualitydefinition` — Quality definitions with `id`, `name`, `title`, `weight`.
- `GET /api/v3/language` — Language profiles with `id`, `name`.

### Download Clients
- `GET /api/v3/downloadclient` — Download client configurations (bare array). Each client has `fields[]` containing `{name, value}` where `value` is a string, number, boolean, or null (decoded as `FlexString`). Root paths are in fields named `downloadFolder` or `tvDownloadFolder`.

### Episode & Episode File
- `GET /api/v3/episode/{id}` — Episode details including `hasFile` (bool) and `episodeFileId` (int, 0 if no file).
- `GET /api/v3/episodefile/{id}` — Episode file details including quality (served as a quality object; decoded as `QualityName`), `customFormatScore` (int), size, season/episode numbers. `customFormatScore` is the basis of the reconciliation upgrade check (§3.2).

### Series
- `GET /api/v3/series/{id}` — Series details (title, path, tvdbId).
- `GET /api/v3/series` — List all series.

---

*End of specification.*
