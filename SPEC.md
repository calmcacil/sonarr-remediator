# Sonarr Recovery Agent — Project Specification

This specification describes the complete implementation. No production testing will occur until all features described here are implemented.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Goals & Non-Goals](#2-goals--non-goals)
3. [Core Features](#3-core-features)
4. [Architecture](#4-architecture)
5. [Component Specifications](#5-component-specifications)
6. [Data Models](#6-data-models)
7. [Rule Engine](#7-rule-engine)
8. [Configuration Schema](#8-configuration-schema)
9. [API Design](#9-api-design)
10. [Dashboard](#10-dashboard)
11. [Notifications](#11-notifications)
12. [Metrics & Observability](#12-metrics--observability)
13. [Testing Strategy](#13-testing-strategy)
14. [Deployment](#14-deployment)
15. [Appendix: Sonarr API Surface](#15-appendix-sonarr-api-surface)

---

## 1. Overview

Sonarr Recovery Agent is a Go microservice that runs alongside Sonarr as a sidecar container. It autonomously detects, analyzes, and recovers from common download and import issues that normally require manual intervention.

It is **not** a cleanup script. It is a continuous recovery agent that observes Sonarr's state, evaluates configurable safety rules, and acts only when it is confident an action is safe.

### Key Characteristics

| Property | Value |
|---|---|
| Language | Go (latest stable; currently 1.26) |
| Runtime | Docker container |
| State | In-memory only (retries, suggestions, decision ring buffer); all source data lives in Sonarr |
| Configuration | YAML file + environment variable overrides with strict validation |
| Observability | Structured JSON logging, Prometheus metrics, health endpoints |
| Safety | Dry-run mode, rule engine gate on every destructive action, human-readable decision logs |

### Relationship with Sonarr

The agent interacts **only** with Sonarr's API. It never communicates with download clients directly. When a queue item needs to be removed, the agent tells Sonarr to remove it; Sonarr forwards the removal to the download client if configured. File scanning for import recovery uses a shared volume mount (read-only).

At startup, the agent detects the running Sonarr version by parsing `GET /api/v3/system/status` (e.g. `"version": "4.0.0.741"` → major version 4) and adapts API call behavior where minor differences exist (blocklist parameter naming, event type values).

### Path Translation

The agent and Sonarr may run in different containers with different mount paths. The agent reads files via its own volume mount (`/data`); Sonarr's API endpoints (parse, manual import) expect paths as seen by Sonarr. The agent maps between the two using the configured `paths.downloadRoots` or paths discovered from Sonarr's download client configuration. All file paths sent to Sonarr's API must be translated to Sonarr's view of the filesystem.

---

## 2. Goals & Non-Goals

### Goals

- Reduce manual Sonarr maintenance.
- Automatically recover from common failure scenarios (stuck downloads, failed imports, naming mismatches).
- Clean up downloads that will never import.
- Assist with edge-case manual imports via confidence-scored suggestions.
- Provide optional web dashboard for visibility and manual approvals.
- Never perform a destructive action without passing all configured safety checks.
- Operate completely autonomously once configured and dry-run is disabled.
- Handle SIGTERM gracefully (finish in-progress operations, flush logs, exit cleanly).

### Non-Goals

- Direct download client management (the agent tells Sonarr; Sonarr tells the client).
- Replacing Sonarr's internal download decision logic.
- Acting as a media manager or indexer.
- Multi-instance coordination (single Sonarr per agent instance).
- Full CRUD management UI for Sonarr.
- Persistent database (in-memory state is sufficient; Sonarr is the source of truth).
- Multi-language support (English only for status message matching and UI).

---

## 3. Core Features

### 3.1 Queue Monitoring

Continuously polls and evaluates every item in the Sonarr download queue and history.

**Data sources polled:**

| Endpoint | Purpose | Interval |
|---|---|---|
| `/api/v3/queue` | Active and queued downloads | 30 s |
| `/api/v3/queue/details` | Per-episode detail (import failures, status messages) | 30 s |
| `/api/v3/history` | Completed/failed import history | 5 min |
| `/api/v3/system/status` | Sonarr health, connectivity, and version | 60 s |

**Tracked state per queue item (composite key: `seriesId:episodeId:downloadId`):**

- Queue status: one of `queued`, `paused`, `downloading`, `completed`, `warning`, `failed`
- Import state (`trackedDownloadState`): one of `downloading`, `importPending`, `importing`, `imported`, `importFailed`, `downloadFailed`
- Status messages: warning text, error text, `trackedDownloadStatus` (`ok`, `warning`, `error`)
- Duration in current state (age tracking)
- Series and episode identifiers for cross-referencing with history

---

### 3.2 Automatic Cleanup of Stuck Downloads

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

**Safety gates:**

- Item age >= configurable minimum (default 2 h)
- Not currently in `importing` state
- No manual import scheduled for this item
- No active retry scheduled for this item
- Rule enabled
- Dry-run check

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

**Safety gates:**

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

**Recovery workflow:**

```
1. DETECT
   Queue item: trackedDownloadState = importFailed.
   History shows: eventType = downloadFailedImport.

2. LOCATE FILES
   Scan the download directory for candidate video files.
   Extensions: .mkv, .mp4, .avi, .m4v, .mov, .wmv, .ts, .iso
   Exclude: sample/, extras/, .nfo, .txt, .jpg, .png, .sfv, .par2
   Walk depth: max 4 levels from download root.

3. PARSE
   Send each candidate file path to Sonarr's parse endpoint.
   Paths must be translated from the agent's container mount to
   Sonarr's container mount before calling the API.
   GET /api/v3/parse?path=/path/as/seen/by/sonarr/video.mkv

4. MATCH
   Cross-reference parsed result with the expected series/episode:
   - Parsed series TVDB ID must match expected series TVDB ID (mismatch = skip)
   - Parsed season must match expected season
   - Parsed episode(s) must contain the expected episode

5. EVALUATE CONFIDENCE (0-100)
   If parse fails or TVDB ID mismatch → 0 (skip entirely).
   Otherwise:
   - TVDB ID match: +35
   - Season matches expected season: +25
   - Episode(s) contain expected episode: +25
   - Quality recognized by Sonarr (parse returned non-zero quality ID): +10
   - Language recognized by Sonarr (parse returned non-zero language ID): +5
   Total: max 100

   Confidence breakdown is logged and exposed in the dashboard/API.

6. PRE-IMPORT CHECK
   a. Call GET /api/v3/episode/{episodeId} to check episode status.
   b. If episode hasFile == false → no existing file, proceed to import.
   c. If episode hasFile == true → call GET /api/v3/episodefile/{episodeFileId}
      to retrieve the existing file's quality.
d. Compare qualities: if the existing file's quality weight is >= the candidate
       quality weight, reject (log and skip). Quality weights are obtained from
       the `QualityDefinition` list fetched at startup (higher weight = better).
       The parse result's `QualityModel.ID` is matched to a `QualityDefinition`
       to get the weight. If the existing file's quality ID is not found in the
       definitions, compare by quality name as a fallback.
   e. For multi-episode files, check each episode individually. Skip episodes
      with equal or better files; import to remaining episodes.

7. IMPORT (if confidence >= threshold)
   For each qualifying episode, call POST /api/v3/manualimport.
   Sonarr's manual import endpoint accepts a single episodeId per request
   for single-episode files. For multi-episode files, make one call per
   episode ID. Request body includes:
   - path (as seen by Sonarr's filesystem)
   - seriesId, seasonNumber
   - episodeId (single int — call once per episode)
   - quality (from parse result), language (from parse result)
   - downloadId (from queue item)
   Import mode is not sent in the request; Sonarr uses its configured default.

8. LOG + NOTIFY
   Record action with full confidence breakdown.
```

**Auto-import thresholds:**

- `confidence >= autoManualImport.minimumConfidence` (default 95): import automatically.
- `confidence < minimumConfidence` but `>= manualReviewThreshold` (default 70): create dashboard suggestion.
- `confidence < manualReviewThreshold`: log only, no action.

---

### 3.5 Manual Import Assistant

When automatic recovery is not confident enough, the system generates import suggestions for manual review via the dashboard.

**Suggestion model:**

```go
type ImportSuggestion struct {
    ID                   string                `json:"id"`
    FilePath             string                `json:"filePath"`
    FileSize             int64                 `json:"fileSize"`
    SeriesTitle          string                `json:"seriesTitle"`
    SeriesID             int                   `json:"seriesId"`
    SeasonNumber         int                   `json:"seasonNumber"`
    EpisodeNumbers       []int                 `json:"episodeNumbers"`
    Confidence           int                   `json:"confidence"`
    ConfidenceBreakdown  *ConfidenceBreakdown  `json:"confidenceBreakdown"`
    MatchDetails         string                `json:"matchDetails"`
    CreatedAt            time.Time             `json:"createdAt"`
    Status               string                `json:"status"`   // pending, approved, rejected, ignored
    DownloadID           string                `json:"downloadId"`
    IgnoreUntil          *time.Time            `json:"ignoreUntil,omitempty"`
}

type ConfidenceBreakdown struct {
    ParseValid       bool `json:"parseValid"`
    TVDBMatch        bool `json:"tvdbMatch"`         // +35
    SeasonMatch      bool `json:"seasonMatch"`       // +25
    EpisodeMatch     bool `json:"episodeMatch"`      // +25
    QualityKnown     bool `json:"qualityKnown"`      // +10
    LanguageKnown    bool `json:"languageKnown"`     // +5
    Total            int  `json:"total"`
}
```

`ParseValid` is always true for items reaching the breakdown screen (failed parses are skipped). It is included for diagnostic transparency.

**Dashboard actions:**

- **Approve**: Re-runs safety check, performs manual import, marks status "approved."
- **Reject**: Marks status "rejected," optionally removes download from queue.
- **Ignore**: Marks status "ignored" for the configured duration (default 24 h). After expiry, the suggestion is re-evaluated.

**Ignore duration** is configurable via `dashboard.ignoreDuration` (default: `24h`).

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
1. Re-checks file existence at expected path.
2. Re-runs parse against Sonarr.
3. Re-attempts manual import.
4. After all retries exhausted, marks permanently failed and fires `import.failed-all-retries` notification.

**Persistence:** In-memory only. If the agent restarts, pending retries are lost.

---

### 3.7 Intelligent Cleanup

Optional background tasks for residual file cleanup.

**Cleanup actions (all configurable, all disabled by default):**

| Action | Description | Safety Check |
|---|---|---|
| `removeEmptyFolders` | Deletes empty directories in download roots | Never deletes root; excludes `.partial`, `_unpack` |
| `removeSampleFiles` | Deletes files matching `sample*`, `*-sample.*` | File size < 500 MB |
| `removeNFOFiles` | Deletes `.nfo` files | File size < 10 MB |
| `removeBrokenSymlinks` | Deletes dangling symlinks | `os.Lstat` check |
| `removeTempExtraction` | Deletes `_unpack`, `.unpack`, `extracted_*` dirs | Only if no active queue items reference paths |
| `removePartialUnpack` | Deletes `.part`, `.partial` files | Only if age > 24 h |

**Schedule:** Configurable interval (default: once per hour).

**Root folder discovery:**
1. If `paths.downloadRoots` is configured, use those paths directly.
2. Otherwise, fetch download client configurations from Sonarr at startup via `GET /api/v3/downloadclient`. Each client's `fields` array is searched for fields named `"downloadFolder"` or `"tvDownloadFolder"` to extract root paths.
3. As a fallback during operation, infer roots from queue item paths.

---

### 3.8 Safety Engine

The central gatekeeper for every destructive action. No action bypasses it.

#### Rule Model

```go
type SafetyRule struct {
    ID          string      `yaml:"id"`
    Description string      `yaml:"description"`
    Conditions  []Condition `yaml:"conditions"`
    Action      Action      `yaml:"action"`
    Enabled     bool        `yaml:"enabled"`
}

type Condition struct {
    Field    string `yaml:"field"`
    Operator string `yaml:"operator"` // eq, neq, gt, gte, lt, lte, in, matches, exists
    Value    string `yaml:"value"`
}

type Action struct {
    Type   string            `yaml:"type"`   // remove_queue, manual_import, retry, cleanup, blocklist
    Params map[string]string `yaml:"params"`
}
```

#### Evaluation Flow

```
For each issue detected:
  1. Match against applicable rules
  2. For each matching rule:
     a. Evaluate ALL conditions (AND logic; short-circuit on first failure)
     b. If all pass -> action approved
  3. Check global constraints (always enforced):
     - Item not already in an active action
     - Cooldown: >= 30 min since last action on same series/episode pair
     - Sonarr connectivity confirmed
     - No duplicate action in last 5 min
     - Item is not in an excluded seriesId or rootPath (prefix match)
  4. Execute action (or log if dry-run)
  5. Log full decision
```

#### Decision Log Format

```json
{
  "timestamp": "2026-07-23T10:12:00Z",
  "decision_id": "dec_abc123",
  "item": {
    "type": "queue_item",
    "id": 420,
    "title": "Ubuntu.S01E05.1080p.WEB-DL",
    "series": "Ubuntu",
    "episode": "S01E05"
  },
  "trigger": "not_custom_format_upgrade",
  "conditions_evaluated": [
    {"field": "status", "operator": "eq", "expected": "completed", "actual": "completed", "passed": true},
    {"field": "age_hours", "operator": "gte", "expected": "2", "actual": "6.3", "passed": true}
  ],
  "action": "remove_from_queue",
  "executed": true,
  "dry_run": false
}
```

### Global Safety Constraints (Always Enforced)

1. **No duplicate actions**: Same item, same rule, within 5 minutes → skip.
2. **Cooldown period**: At least 30 minutes between actions on same series/episode pair (key: `seriesId:episodeId`).
3. **Sonarr connectivity required**: Last health check must have succeeded.
4. **Atomicity**: If first step of multi-step action fails, do not proceed.
5. **Exclusion list**: Any item whose series ID matches `exclusions.seriesIds` or whose root path has a prefix matching any `exclusions.rootPaths` is skipped. Root path matching uses prefix comparison on the item's download path.
6. **State eligibility**: Only items with status in `completed`, `warning`, or `failed` are evaluated.
7. **TVDB ID gate**: In import recovery, if the parsed TVDB ID does not match the expected series TVDB ID, confidence is 0 and the item is skipped. This is enforced before confidence scoring.

### Conflicting Detectors

When multiple detectors flag the same queue item, only the most conservative action is taken. The priority order (most conservative first):

1. `log_only` (cleanup candidate)
2. `remove_queue` (stuck download, not custom format)
3. `blacklist` (blocklist release)
4. `retry` (retry import)
5. `manual_import` (import recovery)

If two detectors propose the same action type, the one with the later `DetectedAt` timestamp is used. The resolution logic is implemented in `internal/safety/builtins.go`.

---

### 3.9 Dry Run Mode

Global flag (`dryRun: true`) that disables all mutating API calls.

**Behavior when enabled:**

- Monitors run normally, including filesystem reads (scanning, path checks).
- Rules evaluate normally.
- Decisions are logged as if the action would occur.
- No `POST`/`DELETE` requests are sent to Sonarr.
- Filesystem mutations (cleanup deletions) are not performed.
- Log entries tagged `"dry_run": true`.
- Dashboard shows "Would have" actions distinctly.

**Purpose:** Deploy the agent, observe behavior, tune rules, build confidence before enabling automation.

---

### 3.10 Dashboard

A minimal embedded web server serving a single-page application.

**Sections:**

| Section | Content |
|---|---|
| **Status Bar** | Connection status, uptime, dry-run indicator, version |
| **Statistics Cards** | Recovered imports, downloads removed, retries performed, pending review count |
| **Current Queue** | Table of queue items with status, age, identified issues |
| **Pending Review** | `ImportSuggestion` items with approve/reject/ignore buttons and confidence breakdown |
| **Recent Activity** | Reverse-chronological feed of decisions (executed or would-have) |
| **Configuration Summary** | Read-only config display (API key masked) |

**Technical implementation:**

- Go `embed.FS` for static assets (single HTML page, vanilla JS, minimal CSS).
- REST API under `/api/` prefix serves all data.
- No external build step, no npm, no SPA framework.
- Auto-refresh via short-polling (5 second interval).
- Optional auth via configurable token (`Authorization: Bearer <token>`).

---

### 3.11 Notifications

Pluggable notification backends. Notifications are **rate-limited per event type** (max 1 notification per event type per 30 minutes, tracked in-memory, resets on restart). Only sent for errors and items requiring human intervention. Routine successful actions produce log entries only.

**Supported integrations:**

| Integration | Configuration |
|---|---|
| Discord Webhook | `notifications.discordWebhook` URL |
| Slack Webhook | `notifications.slackWebhook` URL |
| Gotify | `notifications.gotify` URL + token |
| ntfy | `notifications.ntfy` URL + topic |
| Generic Webhook | `notifications.webhook` URL + method + headers + body template |
| Email (SMTP) | `notifications.email` SMTP settings |

**Notification events:**

| Event | Channels | Purpose |
|---|---|---|
| `import.failed-all-retries` | Discord, Gotify | All retries exhausted — needs human intervention |
| `manual-review.pending` | Discord | New import suggestion needs review |
| `error.sonarr-unreachable` | Gotify, ntfy | Sonarr connectivity lost |

Successful actions (`import.recovered`, `download.removed`, `cleanup.performed`) are logged only, not notified. The dashboard provides a real-time view of all activity.

---

### 3.12 Metrics & Observability

**Prometheus metrics endpoint:** `GET /metrics`

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `sra_imports_recovered_total` | Counter | `confidence_bucket` | Successful auto manual imports |
| `sra_downloads_removed_total` | Counter | `reason` | Downloads removed by rule |
| `sra_retries_total` | Counter | `outcome` | Import retries attempted |
| `sra_cleanup_actions_total` | Counter | `action` | Cleanup actions performed |
| `sra_decisions_evaluated_total` | Counter | `rule`, `passed` | Safety rule evaluations |
| `sra_queue_items_observed` | Gauge | — | Current queue items count |
| `sra_suggestions_pending` | Gauge | — | Pending review suggestions |
| `sra_sonarr_up` | Gauge | — | 1 if Sonarr reachable, 0 otherwise |
| `sra_cycle_duration_seconds` | Histogram | `monitor` | Duration per monitoring cycle |

**Health endpoints:**

| Endpoint | Purpose |
|---|---|
| `GET /health` | Returns `200` if agent is running |
| `GET /health/sonarr` | Returns `200` if Sonarr is reachable and authenticated |

**Logging:**

- Structured JSON logs to stdout (Docker-native).
- Levels: `debug`, `info`, `warn`, `error`.
- Fields: `timestamp`, `level`, `component`, `message`, `item`, `rule`, `action`, `dry_run`, `error`.

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
  |  +------------------+  +------------------+  +------------------+   |
  |  | Queue Monitor    |  | History Monitor  |  | Health Monitor   |   |
  |  | (30s interval)   |  | (5min interval)  |  | (60s interval)   |   |
  |  +--------+---------+  +--------+---------+  +--------+---------+   |
  |           |                      |                      |            |
  |           +----------------------+----------------------+            |
  |                                  |                                   |
  |                        +---------v---------+                        |
  |                        |    Issue Detector  |                        |
  |                        +---------+---------+                        |
  |                                  |                                   |
  |           +----------------------+----------------------+            |
  |           |                      |                      |            |
  |  +--------v---------+  +--------v---------+  +--------v---------+   |
  |  | Stuck Download   |  | Not Custom Fmt   |  | Import Recovery  |   |
  |  | Detector         |  | Upgrade Detector |  | Detector         |   |
  |  +--------+---------+  +--------+---------+  +--------+---------+   |
  |           |                      |                      |            |
  |           +----------------------+----------------------+            |
  |                                  |                                   |
  |                        +---------v---------+                        |
  |                        |   Safety Engine    |                        |
  |                        |  (Rule Evaluator)  |                        |
  |                        +---------+---------+                        |
  |                                  |                                   |
  |           +----------------------+----------------------+            |
  |           |                      |                      |            |
  |  +--------v---------+  +--------v---------+  +--------v---------+   |
  |  | Action Executor  |  | Retry Scheduler  |  | Cleanup Engine   |   |
  |  +--------+---------+  +--------+---------+  +--------+---------+   |
  |           |                      |                      |            |
  |           +----------------------+----------------------+            |
  |                                  |                                   |
  |  +-------------------------------+-------------------------------+  |
  |  |                                                                |  |
  |  |  +----------------+  +----------------+  +------------------+  |  |
  |  |  | Decision Logger|  | Notifier       |  | Metrics Exporter |  |  |
  |  |  +----------------+  +----------------+  +------------------+  |  |
  |  |                                                                |  |
  |  |  +----------------+  +----------------+                        |  |
  |  |  | Dashboard API  |  | Config Loader  |                        |  |
  |  |  +-------+--------+  +----------------+                        |  |
  |  +----------+-----------------------------------------------------+  |
  |             |                                                         |
  +-------------+---------------------------------------------------------+
                |
          Web Dashboard
           (browser)
```

### Internal Component Communication

- **Monitors** push detected items into per-type channels.
- **Detectors** consume from monitor channels and produce `Issue` values.
- **Safety Engine** receives `Issue` values, evaluates rules, and produces `Decision` values.
- **Action Executor** receives approved `Decision` values and performs Sonarr API calls (or logs as dry-run).
- **Retry Scheduler** manages an in-memory timer-based retry queue.
- **Dashboard API** exposes shared aggregations (last N decisions, stats, suggestions).
- **Cleanup Engine** uses shared volume mount for filesystem access.

### Directory Structure

```
sonarr-remediator/
├── cmd/
│   └── sonarr-remediator/
│       └── main.go              # Entry point, config loading, service wiring
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
│   │   ├── parse.go             # Parse endpoint calls (with path translation)
│   │   ├── system.go            # System status, health, version
│   │   ├── quality.go           # Quality definitions (fetched at startup)
│   │   ├── language.go          # Language definitions (fetched at startup)
│   │   └── download_client.go   # Download client root folder discovery
│   ├── monitors/
│   │   ├── queue_monitor.go     # Queue polling & diffing (composite key)
│   │   ├── history_monitor.go   # History polling & diffing
│   │   └── health_monitor.go    # Sonarr connectivity
│   ├── detectors/
│   │   ├── detector.go          # Detector interface
│   │   ├── stuck_download.go    # Stuck download detection
│   │   ├── not_custom_format.go # "Not custom format upgrade" detection
│   │   ├── import_recovery.go   # Failed import recovery detection
│   │   └── cleanup.go           # Cleanup candidate detection
│   ├── recovery/
│   │   ├── import.go            # File scanning, parse, confidence, import
│   │   └── scanner.go           # Directory walking & file matching
│   ├── safety/
│   │   ├── engine.go            # Rule evaluation engine + global constraints
│   │   ├── rule.go              # Rule & condition types
│   │   └── builtins.go          # Built-in rules + detector conflict resolution
│   ├── executor/
│   │   ├── executor.go          # Action execution interface
│   │   ├── queue_actions.go     # Queue removal (via Sonarr API)
│   │   ├── import_actions.go    # Manual import (via Sonarr API)
│   │   ├── cleanup_actions.go   # Filesystem cleanup
│   │   └── retry.go             # Retry scheduling & execution
│   ├── scheduler/
│   │   └── scheduler.go         # Periodic task scheduler
│   ├── dashboard/
│   │   ├── server.go            # HTTP server & router
│   │   ├── api.go               # Dashboard REST API handlers
│   │   ├── auth.go              # Token auth middleware
│   │   └── assets/
│   │       ├── index.html       # Dashboard SPA
│   │       ├── style.css        # Minimal styling
│   │       └── app.js           # Dashboard logic (vanilla JS)
│   ├── notifications/
│   │   ├── notifier.go          # Notifier interface + in-memory rate limiter
│   │   ├── discord.go
│   │   ├── slack.go
│   │   ├── gotify.go
│   │   ├── ntfy.go
│   │   └── webhook.go
│   ├── metrics/
│   │   └── metrics.go           # Prometheus metrics
│   ├── logging/
│   │   └── logging.go           # Structured logger (slog)
│   └── types/
│       └── types.go             # Domain types (Issue, Decision, Suggestion)
├── config.example.yaml
├── Dockerfile
├── docker-compose.example.yaml
├── go.mod
├── go.sum
├── Makefile
├── README.md
└── SPEC.md
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
func (c *Client) GetQueueDetails(ctx context.Context) ([]QueueDetailItem, error)
func (c *Client) GetHistory(ctx context.Context, params HistoryParams) ([]HistoryItem, error)
func (c *Client) GetSystemStatus(ctx context.Context) (SystemStatus, error)
func (c *Client) RemoveQueueItem(ctx context.Context, id int, blocklist bool) error
func (c *Client) Parse(ctx context.Context, path string) (*ParseResult, error)
func (c *Client) ManualImport(ctx context.Context, req ManualImportRequest) error
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
- `401`/`403` responses trigger an auth failure alert; monitors continue disabled until credentials are fixed.
- Other `4xx` (except `429`) are terminal errors for that item.
- `5xx` and network errors trigger exponential backoff with jitter (max 3 retries).
- Concurrent request limiting via semaphore (max 5 concurrent requests).
- If Sonarr becomes unreachable, monitors pause with exponential backoff (1 min, 2 min, 4 min, ..., max 10 min). Once connectivity resumes, perform a full state refresh.

---

### 5.2 Queue Monitor (`internal/monitors/queue_monitor.go`)

```go
type QueueMonitor struct {
    client   *sonarr.Client
    interval time.Duration
    issues   chan<- Issue
    lastSeen map[string]QueueState  // key: "seriesId:episodeId:downloadId"
}
```

Each tick: fetch queue + details, diff against lastSeen using composite keys, emit Issues for items in eligible states.

---

### 5.3 Issue Detector Interface (`internal/detectors/detector.go`)

```go
type Detector interface {
    Name() string
    Detect(ctx context.Context, item QueueItem, history []HistoryItem) (*Issue, error)
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
    IssueStuckDownload    IssueType = "stuck_download"
    IssueNotCustomFormat  IssueType = "not_custom_format_upgrade"
    IssueImportFailed     IssueType = "import_failed"
    IssueCleanupCandidate IssueType = "cleanup_candidate"
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
    rules       []SafetyRule
    activeItems map[string]time.Time   // active actions (composite key)
    lastAction  map[string]time.Time   // cooldown ("seriesId:episodeId")
}

func (e *Engine) Evaluate(ctx context.Context, issue Issue) (*Decision, error)

type Decision struct {
    Issue      Issue             `json:"issue"`
    Rule       SafetyRule        `json:"rule"`
    Action     Action            `json:"action"`
    Conditions []ConditionResult `json:"conditions"`
    Approved   bool              `json:"approved"`
    Reason     string            `json:"reason,omitempty"`
    Timestamp  time.Time         `json:"timestamp"`
    DryRun     bool              `json:"dryRun"`
}

type ConditionResult struct {
    Field    string `json:"field"`
    Operator string `json:"operator"`
    Expected string `json:"expected"`
    Actual   string `json:"actual"`
    Passed   bool   `json:"passed"`
}
```

---

### 5.5 Action Executor (`internal/executor/executor.go`)

```go
type Executor struct {
    sonarrClient *sonarr.Client
    notifier     *notifications.Notifier
    dryRun       bool
}

func (e *Executor) Execute(ctx context.Context, decision Decision) error
```

Each handler:
1. Checks `dryRun` — if true, logs and returns nil.
2. Performs the Sonarr API call(s) or filesystem operation.
3. Logs the outcome.
4. Fires notification if configured (only for error states; see §3.11).

---

## 6. Data Models

### Core Domain Types

```go
// ─── Queue ───────────────────────────────────────────────────────────

type QueueItem struct {
    ID                    int             `json:"id"`
    SeriesID              int             `json:"seriesId"`
    EpisodeID             int             `json:"episodeId"`
    SeriesTitle           string          `json:"seriesTitle"`
    EpisodeTitle          string          `json:"episodeTitle"`
    Quality               string          `json:"quality"`
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

type StatusMessage struct {
    Title    string   `json:"title"`
    Messages []string `json:"messages"`
}

type QueueDetailItem struct {
    QueueItem
    Episode *EpisodeResource `json:"episode,omitempty"`
}

// ─── History ─────────────────────────────────────────────────────────

type HistoryItem struct {
    ID          int               `json:"id"`
    SeriesID    int               `json:"seriesId"`
    EpisodeID   int               `json:"episodeId"`
    SourceTitle string            `json:"sourceTitle"`
    EventType   string            `json:"eventType"`  // "grabbed"|"downloadFolderImported"|"downloadFailedImport"|"downloadIgnored"|"episodeFileDeleted"
    Quality     string            `json:"quality"`
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

type ManualImportRequest struct {
    Path         string        `json:"path"`
    SeriesID     int           `json:"seriesId"`
    SeasonNumber int           `json:"seasonNumber"`
    EpisodeID    int           `json:"episodeId"`   // single episode per call; issue multiple calls for multi-ep files
    Quality      QualityModel  `json:"quality"`
    Language     LanguageModel `json:"language"`
    DownloadID   string        `json:"downloadId"`
    // ImportMode is intentionally not included; Sonarr uses its configured default.
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
    ID                  int    `json:"id"`
    SeriesID            int    `json:"seriesId"`
    SeasonNumber        int    `json:"seasonNumber"`
    EpisodeNumber       int    `json:"episodeNumber"`
    RelativePath        string `json:"relativePath"`
    Quality             string `json:"quality"`
    QualityCutoffNotMet bool   `json:"qualityCutoffNotMet"`
    Size                int64  `json:"size"`
}
// Note: Sonarr's episode file API response does not include a quality ID directly.
// Quality comparison is done by fetching the episode file's quality definitions
// or by maintaining a quality-to-ID mapping from the quality definitions endpoint.
// For simplicity, the pre-import check compares quality by name: if the existing
// file's quality name matches or is ranked higher in the quality definitions list,
// the import is rejected.

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
    ID     int                     `json:"id"`
    Name   string                  `json:"name"`
    Fields []DownloadClientField   `json:"fields"`
}

type DownloadClientField struct {
    Name  string `json:"name"`
    Value string `json:"value"`
}
// Root folder extraction: iterate Fields, look for Name == "downloadFolder"
// or Name == "tvDownloadFolder". The Value is the download root path (string).

// ─── System ──────────────────────────────────────────────────────────

type SystemStatus struct {
    Version string `json:"version"`  // e.g. "4.0.0.741"
}
```

---

## 7. Rule Engine

### Built-in Rules (Derived from Config)

Each automation setting generates internal safety rules at startup.

**Example:**

```yaml
automation:
  removeNotCustomFormat:
    enabled: true
    waitHours: 2
```

Generates:

```go
SafetyRule{
    ID:          "remove_not_custom_format",
    Description: "Remove completed downloads that Sonarr determined are not a custom format upgrade",
    Enabled:     true,
    Conditions: []Condition{
        {Field: "queue.status", Operator: "eq", Value: "completed"},
        {Field: "queue.trackedDownloadState", Operator: "eq", Value: "importFailed"},
        {Field: "status_message", Operator: "matches", Value: "(?i)not.*(custom format|an upgrade)"},
        {Field: "age_hours", Operator: "gte", Value: "2"},
        {Field: "currently_importing", Operator: "eq", Value: "false"},
    },
    Action: Action{
        Type:   "remove_from_queue",
        Params: map[string]string{"blocklist": "false"},
    },
}
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
  historyInterval: 5m
  healthInterval: 60s
  startupDelay: 10s

# ─── File System ───
paths:
  downloadRoots: []
  # If empty, fetched from Sonarr's download client configuration at startup.
  # If the agent container's mount path differs from Sonarr's, provide explicit mappings.
  agentRoot: ""     # The root path as seen by the agent container (e.g., /data)
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
    minimumConfidence: 95
    manualReviewThreshold: 70

  cleanup:
    enabled: false
    interval: 1h
    actions:
      removeEmptyFolders:
        enabled: false
        excludePatterns:
          - "*.partial"
          - "_unpack*"
      removeSampleFiles:
        enabled: false
        maxSizeMB: 500
        patterns:
          - "**/sample*"
          - "**/*-sample.*"
      removeNFOFiles:
        enabled: false
        maxSizeMB: 10
      removeBrokenSymlinks:
        enabled: false
      removeTempExtraction:
        enabled: false
        ageHours: 24
        patterns:
          - "**/_unpack/**"
          - "**/.unpack/**"
          - "**/extracted_*/**"

# ─── Dashboard ───
dashboard:
  enabled: true
  port: 8080
  host: 0.0.0.0
  authToken: ""
  ignoreDuration: 24h               # Duration a suggestion stays "ignored"

# ─── Notifications ───
notifications:
  discordWebhook: ""
  slackWebhook: ""
  gotify:
    url: ""
    token: ""
    priority: 5
  ntfy:
    url: ""
    topic: ""
    token: ""
    priority: 3
  webhook:
    url: ""
    method: POST
    headers: {}
    bodyTemplate: ""                # Go template; context is the notification event struct
  email:
    enabled: false
    smtpHost: ""
    smtpPort: 587
    smtpUsername: ""
    smtpPassword: ""
    from: ""
    to: []

  events:
    import.failed-all-retries: [discord, gotify]
    manual-review.pending: [discord]
    error.sonarr-unreachable: [gotify, ntfy]

# ─── Logging ───
logging:
  level: info
  format: json

# ─── Global ───
dryRun: true
```

### Path Translation

When `paths.agentRoot` and `paths.sonarrRoot` are both set, the agent translates paths before sending them to Sonarr's API. For example, if the agent sees `/data/downloads/movie.mkv` and `agentRoot=/data`, `sonarrRoot=/data`, the path sent to Sonarr is `/data/downloads/movie.mkv` (unchanged when roots match). If roots differ, the prefix is swapped.

### Environment Variable Overrides

Prefix `SRA_` with double-underscore separators:

```
SRA_SONARR__URL=http://sonarr:8989
SRA_SONARR__API_KEY=abc123
SRA_DRY_RUN=false
SRA_LOGGING__LEVEL=debug
SRA_DASHBOARD__PORT=9090
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
| `autoManualImport.manualReviewThreshold` | Must be 0-100 and <= `minimumConfidence` |
| `dashboard.ignoreDuration` | Must be a valid Go duration > 0 |
| `retryImports.retryIntervals` | Must be non-empty if `retryImports.enabled` |
| `notifications.email` | If enabled, all SMTP fields required |
| `paths.downloadRoots` | If provided, each path must exist and be readable |
| `paths.agentRoot` and `paths.sonarrRoot` | If one is set, both must be set; both must exist |
| `exclusions.seriesIds` | Each ID must be a positive integer |
| `exclusions.rootPaths` | If provided, each path must exist |
| Config file | Must parse as valid YAML with no unknown top-level keys |

---

## 9. API Design

### Internal REST API (Dashboard Backend)

Base path: `/api/v1/`

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/status` | Agent status (version, uptime, dry-run, sonarr status) |
| `GET` | `/api/v1/stats` | Aggregated statistics |
| `GET` | `/api/v1/queue` | Current queue items (cached, from last poll) |
| `GET` | `/api/v1/suggestions` | List pending import suggestions |
| `POST` | `/api/v1/suggestions/{id}/approve` | Approve suggestion (triggers import) |
| `POST` | `/api/v1/suggestions/{id}/reject` | Reject suggestion |
| `POST` | `/api/v1/suggestions/{id}/ignore` | Ignore suggestion for `dashboard.ignoreDuration` |
| `GET` | `/api/v1/activity` | Recent decisions feed (limit/offset) |
| `GET` | `/api/v1/config` | Read-only config summary (API key masked) |
| `GET` | `/health` | Health check |
| `GET` | `/health/sonarr` | Sonarr connectivity check |
| `GET` | `/metrics` | Prometheus metrics endpoint |

### Authentication

Dashboard endpoints protected by token when `dashboard.authToken` is set:

```
Authorization: Bearer <authToken>
```

`/health*` and `/metrics` endpoints are not protected.

---

## 10. Dashboard

### Technology

- Single HTML file with embedded CSS and JavaScript.
- Zero external dependencies.
- Served via Go's `embed.FS`.

### UI Layout

```
┌─────────────────────────────────────────────────┐
│  Sonarr Recovery Agent                         │
│  ✓ Connected | Uptime: 3d 4h | Dry Run: ON     │
├──────────┬──────────┬──────────┬────────────────┤
│ Recovered│ Downloads│ Pending  │ Retries        │
│ Imports  │ Removed  │ Review   │                │
│    5     │   14     │    2     │     3          │
├──────────┴──────────┴──────────┴────────────────┤
│                                                 │
│  Recent Activity                                │
│  ┌─────────────────────────────────────────────┐│
│  │ 10:12  Removed: Ubuntu.S01E05              ││
│  │        Reason: Not Custom Format Upgrade   ││
│  ├─────────────────────────────────────────────┤│
│  │ 09:42  Recovered: Breaking Bad S05E14      ││
│  │        Confidence: 98%  TVDB✓ S✓ E✓ Q✓ L✓  ││
│  ├─────────────────────────────────────────────┤│
│  │ 09:15  Suggestion: Saul S06E03  85%        ││
│  │        TVDB✓ S✓ E✗  [Approve] [Reject]     ││
│  └─────────────────────────────────────────────┘│
└─────────────────────────────────────────────────┘
```

---

## 11. Notifications

### Templates

**`manual-review.pending` (Discord):**

```json
{
  "embeds": [{
    "title": "Import Suggestion Needs Review",
    "description": "**Series:** Foundation\n**Episode:** S02E08\n**Confidence:** 85%\n**Breakdown:** TVDB ✓ | Season ✓ | Episode ✓ | Quality ✓ | Language ✓",
    "color": 16705372,
    "footer": {"text": "Sonarr Recovery Agent — Manual Review Required"}
  }]
}
```

**`import.failed-all-retries` (Discord):**

```json
{
  "embeds": [{
    "title": "Import Permanently Failed",
    "description": "**Series:** Foundation\n**Episode:** S02E08\nAll retries exhausted. Manual intervention required.",
    "color": 15548997,
    "footer": {"text": "Sonarr Recovery Agent — Action Required"}
  }]
}
```

**`error.sonarr-unreachable` (Gotify/ntfy):**

```
Title: Sonarr Unreachable
Message: Sonarr at http://sonarr:8989 is not responding. Agent will retry with backoff.
Priority: high
```

---

## 12. Metrics & Observability

All metrics under the `sra_` namespace.

```
# HELP sra_imports_recovered_total Successful automatic manual imports.
# TYPE sra_imports_recovered_total counter
sra_imports_recovered_total{confidence_bucket="95-100"} 5

# HELP sra_downloads_removed_total Downloads removed by rule.
# TYPE sra_downloads_removed_total counter
sra_downloads_removed_total{reason="not_custom_format"} 14

# HELP sra_retries_total Import retries attempted.
# TYPE sra_retries_total counter
sra_retries_total{outcome="success"} 2

# HELP sra_decisions_evaluated_total Safety rule evaluations.
# TYPE sra_decisions_evaluated_total counter
sra_decisions_evaluated_total{rule="remove_not_custom_format",passed="true"} 42

# HELP sra_queue_items_observed Current queue items count.
# TYPE sra_queue_items_observed gauge
sra_queue_items_observed 18

# HELP sra_suggestions_pending Pending review suggestions.
# TYPE sra_suggestions_pending gauge
sra_suggestions_pending 2

# HELP sra_sonarr_up 1 if Sonarr reachable, 0 otherwise.
# TYPE sra_sonarr_up gauge
sra_sonarr_up 1

# HELP sra_cycle_duration_seconds Duration per monitoring cycle.
# TYPE sra_cycle_duration_seconds histogram
sra_cycle_duration_seconds_bucket{monitor="queue",le="0.5"} 100
```

---

## 13. Testing Strategy

### Unit Tests

| Layer | Coverage Target | Focus |
|---|---|---|
| `internal/safety/` | 95%+ | Rule evaluation, condition logic, conflict resolution |
| `internal/recovery/` | 90%+ | Confidence scoring (TVDB-gate), file matching, parse interpretation |
| `internal/config/` | 90%+ | Config loading, env overrides, validation |
| `internal/detectors/` | 85%+ | Issue detection with mocked Sonarr data |
| `internal/executor/` | 85%+ | Action dispatch, dry-run, error handling |
| `internal/notifications/` | 80%+ | Template rendering, rate limiting, event routing |

### Integration Tests

**Sonarr API Mock:** `httptest.Server` simulating Sonarr API responses for all endpoints.

**End-to-end scenarios:**
1. Stuck download detected → safety check passes → removal (or dry-run skip).
2. "Not a Custom Format Upgrade" detected via queue message → removal.
3. "Not a Custom Format Upgrade" detected via history event → removal.
4. Failed import → TVDB match → high confidence → auto-import.
5. Failed import → TVDB mismatch → confidence 0 → skip.
6. Failed import → TVDB match → medium confidence → suggestion created with breakdown.
7. Failed import → transient error → retry schedule → recovery on retry N.
8. Failed import → all retries exhausted → `import.failed-all-retries` notification.
9. Multi-episode file → import via multiple `episodeId` calls.
10. Pre-import check finds existing better file → import rejected.
11. Pre-import check: existing file has lower quality → import proceeds.
12. Pre-import check: episode has no file → import proceeds.
13. Dry-run enabled → all detections work, zero mutations.
14. Exclusion list (seriesId) match → item skipped entirely.
15. Exclusion list (rootPath prefix) match → item skipped entirely.
16. Path translation: agent path correctly mapped to Sonarr path.
17. Dashboard ignore → suggestion returns after `ignoreDuration` expires.

### Test Fixtures

Real anonymized Sonarr API responses stored as JSON fixtures.

---

## 14. Deployment

### Docker

```dockerfile
FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /sonarr-remediator ./cmd/sonarr-remediator

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /sonarr-remediator /usr/local/bin/sonarr-remediator
EXPOSE 8080
USER 1000:1000
ENTRYPOINT ["sonarr-remediator"]
CMD ["--config", "/config/config.yaml"]
```

### Docker Compose (with Sonarr)

```yaml
services:
  sonarr:
    image: linuxserver/sonarr:latest
    volumes:
      - ./sonarr/config:/config
      - ./data:/data

  sonarr-remediator:
    image: ghcr.io/calmcacil/sonarr-remediator:latest
    volumes:
      - ./data:/data:ro
      - ./remediator-config:/config
    environment:
      - SRA_SONARR__URL=http://sonarr:8989
      - SRA_SONARR__API_KEY=${SONARR_API_KEY}
      - SRA_DRY_RUN=true
    ports:
      - "8080:8080"
    depends_on:
      - sonarr
    stop_grace_period: 30s
```

### Graceful Shutdown

On receiving `SIGTERM` or `SIGINT`:

1. Stop all monitors (no new poll cycles).
2. Cancel any pending cleanup operations.
3. Allow in-progress manual imports and Sonarr API calls to complete (timeout: 30 s).
4. Flush decision log ring buffer to stdout.
5. Drain and close notification channels.
6. Close HTTP server.
7. Exit with code 0.

`stop_grace_period` should be at least 30 seconds. A second signal causes immediate exit.

---

## 15. Appendix: Sonarr API Surface

The agent uses the Sonarr v3 API (used by both Sonarr v3 and v4 installations). The agent detects the running version at startup and adapts where minor differences exist.

### Queue
- `GET /api/v3/queue` — List queue items.
- `GET /api/v3/queue/details` — Queue items with episode details.
- `DELETE /api/v3/queue/{id}` — Remove item. Params: `blocklist=true` (v3) or `removeFromClient=true` (v4 fallback, version-detected).

### History
- `GET /api/v3/history` — Event history. Params: `page`, `pageSize`, `sortKey`, `sortDirection`, `eventType` (int: 1=grabbed, 3=imported, 4=failedImport, 7=ignored), `episodeId`, `seriesId`.

### Manual Import
- `GET /api/v3/manualimport` — Files available for manual import.
- `POST /api/v3/manualimport` — Execute manual import. Accepts `episodeId` (single int).

### Parse
- `GET /api/v3/parse` — Parse a file path or title. Param: `path` or `title`.

### System
- `GET /api/v3/system/status` — System status. Response includes `version` (string).

### Quality & Language
- `GET /api/v3/qualitydefinition` — Quality definitions with `id`, `name`, `title`, `weight`.
- `GET /api/v3/language` — Language profiles with `id`, `name`.

### Download Clients
- `GET /api/v3/downloadclient` — Download client configurations. Each client has `fields[]` containing `{name, value}`. Root paths are in fields named `downloadFolder` or `tvDownloadFolder`.

### Episode & Episode File
- `GET /api/v3/episode/{id}` — Episode details including `hasFile` (bool) and `episodeFileId` (int, 0 if no file).
- `GET /api/v3/episodefile/{id}` — Episode file details including quality name, size, season/episode numbers.

### Series
- `GET /api/v3/series/{id}` — Series details (title, path, tvdbId).
- `GET /api/v3/series` — List all series.

---

*End of specification.*
