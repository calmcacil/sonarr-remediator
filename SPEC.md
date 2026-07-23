# Sonarr Recovery Agent — Project Specification

**Version:** 2.0.0
**Status:** Draft

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
15. [Phase Roadmap](#15-phase-roadmap)
16. [Long-Term Vision](#16-long-term-vision)
17. [Appendix: Sonarr API Surface](#17-appendix-sonarr-api-surface)

---

## 1. Overview

Sonarr Recovery Agent is a Go microservice that runs alongside Sonarr as a sidecar container. Its purpose is to autonomously detect, analyze, and recover from common download and import issues that normally require manual intervention.

It is **not** a cleanup script. It is a continuous recovery agent that observes Sonarr's state, evaluates configurable safety rules, and acts only when it is confident an action is safe.

### Key Characteristics

| Property | Value |
|---|---|
| Language | Go (latest stable; currently 1.26) |
| Runtime | Docker container |
| State | Stateless in-memory state (retries, suggestions, decision ring buffer); all source data lives in Sonarr |
| Configuration | YAML file + environment variable overrides |
| Observability | Structured JSON logging, Prometheus metrics, health endpoints |
| Safety | Dry-run mode, rule engine gate on every destructive action, human-readable decision logs |

### Relationship with Sonarr

The agent interacts **only** with Sonarr's API. It never communicates with download clients directly. When a queue item needs to be removed, the agent tells Sonarr to remove it; Sonarr forwards the removal to the download client if configured. File scanning for import recovery uses a shared volume mount (read-only).

---

## 2. Goals & Non-Goals

### Goals

- Reduce manual Sonarr maintenance.
- Automatically recover from common failure scenarios (stuck downloads, failed imports, naming mismatches).
- Clean up downloads that will never import ("Not a Custom Format Upgrade" pattern).
- Assist with edge-case manual imports via confidence-scored suggestions.
- Provide optional web dashboard for visibility and manual approvals.
- Never perform a destructive action without passing all configured safety checks.
- Operate completely autonomously once configured and dry-run is disabled.

### Non-Goals (v1)

- Direct download client management (the agent tells Sonarr; Sonarr tells the client).
- Replacing Sonarr's internal download decision logic.
- Acting as a media manager or indexer.
- Multi-instance coordination (single Sonarr per agent instance).
- Full CRUD management UI for Sonarr (this is a recovery agent, not a Sonarr client).
- Persistent database (in-memory state is sufficient; Sonarr is the source of truth).

---

## 3. Core Features

### 3.1 Queue Monitoring

Continuously polls and evaluates every item in the Sonarr download queue and history.

**Data sources polled:**

| Endpoint | Purpose | Interval |
|---|---|---|
| `/api/v3/queue` | Active and queued downloads (including warnings, errors, status messages) | 30 s |
| `/api/v3/queue/details` | Per-episode granular detail (import failures, status messages) | 30 s |
| `/api/v3/history` | Completed/failed import history | 5 min |
| `/api/v3/system/status` | Sonarr health and connectivity | 60 s |

**Tracked state per queue item:**

- Download status: queued / paused / downloading / completed / warning / failed
- Import state: pending / importing / imported / importFailed
- Status messages: warning text, error text, tracked download status
- Duration in current state (age tracking)
- Episode/series identifiers for cross-referencing with history

---

### 3.2 Automatic Cleanup of Stuck Downloads

Detects downloads that have become permanently stuck and will never import successfully.

**Trigger conditions (any single one is sufficient):**

| Condition | Detection |
|---|---|
| Sonarr reports error on queue item | Queue item has `errorMessage` or `trackedDownloadStatus` = `error` |
| Missing files | Status message contains "No files found are eligible for import" |
| Sonarr abandoned item | Queue item is `completed` but history shows no import attempt after configurable timeout (default 6 h) |
| Configurable age timeout | Time since download completed > `maxAge` AND no import in progress |
| Import repeatedly failed | History shows N consecutive `downloadFailedImport` events for same episode |

**Actions (ordered, all optional):**

1. Remove from queue (`DELETE /api/v3/queue/{id}`)
2. Remove from queue with blocklist (`DELETE /api/v3/queue/{id}?blocklist=true`)
3. Log only (no mutation)

**Safety gates:**

- Item age ≥ configurable minimum (default 2 h)
- Not currently in "importing" state
- No manual import scheduled for this item
- No active retry scheduled for this item
- Rule enabled check
- Dry-run check

---

### 3.3 "Not a Custom Format Upgrade" Removal

One of the most common Sonarr patterns: a download completes, Sonarr determines it is not an upgrade over the existing file (based on custom format scoring), but leaves the completed download sitting in the queue.

**Detection strategy (both methods used):**

**Method A — Queue status message parsing:**
- Queue item has `trackedDownloadStatus` = `warning`
- Status message text matches: `"Not a Custom Format Upgrade"` or configurable regex

**Method B — History event inspection:**
- History contains `eventType` = `downloadIgnored`
- History item data includes a reason matching "Not an upgrade"

If either method matches, the item is flagged as "not an upgrade."

**Safety gates:**

| Condition | Source |
|---|---|
| Download completed | Queue status |
| Import decision confirmed as no-upgrade | Status message OR history event |
| Age ≥ waitHours (default 2) | `automation.removeNotCustomFormat.waitHours` |
| Not currently importing | Queue item trackedDownloadState |
| No active retry scheduled for this item | In-memory retry queue check |
| No other active queue item for same episode | Queue + episode ID check |
| Rule enabled | `automation.removeNotCustomFormat.enabled` |

**Default action:**
Remove from queue via Sonarr API. Blocklisting is optional via config.

---

### 3.4 Import Recovery

For downloads where the download completed but Sonarr could not automatically match and import the video file(s).

**Common failure causes:**

- Strange or obfuscated release names
- Download client renamed the folder
- Multi-file releases (CD1/CD2, sample + main, extras)
- Anime absolute numbering (TVDB-based, but naming varies)
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
   Send each candidate file path to Sonarr's parse endpoint:
   GET /api/v3/parse?path=/absolute/path/to/file

   Sonarr returns parsed metadata: series title, season number, episode numbers,
   absolute episode numbers, quality, language.

4. MATCH
   Cross-reference parsed result with the expected series/episode from the
   original queue item:
   - Parsed series TVDB ID matches expected series TVDB ID
   - Parsed season matches expected season
   - Parsed episode(s) contain the expected episode

5. EVALUATE CONFIDENCE
   Compute a confidence score (0-100) with full breakdown:
   - Sonarr parse returned valid result: base score, or zero (skip entirely)
   - TVDB ID match: +40
   - Season match: +20
   - Episode match: +20
   - Quality recognized by Sonarr: +10
   - Language recognized by Sonarr: +5
   - File size plausible (> 50 MB): +5

6. IMPORT (if confidence >= threshold)
   POST /api/v3/manualimport
   Body includes: path, seriesId, seasonNumber, episodeIds (supports array
   for multi-ep files), quality, language, downloadId, importMode=move.

7. LOG + NOTIFY
   Record action with full confidence breakdown.
```

**Auto-import thresholds:**

- `confidence >= autoManualImport.minimumConfidence` (default 95): import automatically.
- `confidence < minimumConfidence` but `>= manualReviewThreshold` (default 70): create dashboard suggestion for manual review.
- `confidence < manualReviewThreshold`: take no action, log only.

---

### 3.5 Manual Import Assistant

When automatic recovery is not confident enough, the system generates import suggestions for manual review.

**Suggestion model:**

```go
type ImportSuggestion struct {
    ID              string              `json:"id"`
    FilePath        string              `json:"filePath"`
    FileSize        int64               `json:"fileSize"`
    SeriesTitle     string              `json:"seriesTitle"`
    SeriesID        int                 `json:"seriesId"`
    SeasonNumber    int                 `json:"seasonNumber"`
    EpisodeNumbers  []int               `json:"episodeNumbers"`   // supports multi-episode
    Confidence      int                 `json:"confidence"`       // 0-100
    ConfidenceBreakdown *ConfidenceBreakdown `json:"confidenceBreakdown"`
    MatchDetails    string              `json:"matchDetails"`     // human-readable explanation
    CreatedAt       time.Time           `json:"createdAt"`
    Status          string              `json:"status"`           // pending, approved, rejected, ignored
    DownloadID      string              `json:"downloadId"`
}

type ConfidenceBreakdown struct {
    ParseSuccess    bool   `json:"parseSuccess"`
    TVDBMatch       bool   `json:"tvdbMatch"`       // +40
    SeasonMatch     bool   `json:"seasonMatch"`     // +20
    EpisodeMatch    bool   `json:"episodeMatch"`    // +20
    QualityKnown    bool   `json:"qualityKnown"`    // +10
    LanguageKnown   bool   `json:"languageKnown"`   // +5
    FileSizePlausible bool `json:"fileSizePlausible"` // +5
    Total           int    `json:"total"`
}
```

**Dashboard actions:**

- **Approve**: Re-runs safety check, performs `POST /api/v3/manualimport`, marks status "approved."
- **Reject**: Marks status "rejected," optionally removes download from queue.
- **Ignore**: Marks status "ignored" for N hours (configurable, default 24).

---

### 3.6 Retry Failed Imports

Re-queues imports that failed due to transient conditions.

**Retryable failure signatures (regex-matched against error messages):**

| Pattern | Reason |
|---|---|
| `(?i)permission denied` | NAS/FS permission issue |
| `(?i)access denied` | NAS/FS access issue |
| `(?i)no such file` | NAS/SMB temporarily unavailable |
| `(?i)connection refused` | Service unavailable |
| `(?i)connection timed out` | Network issue |
| `(?i)no space left` | Disk full |
| `(?i)input/output error` | Transient I/O |
| `(?i)file.*in use` | File locked by another process |
| `(?i)destination.*locked` | Destination locked |
| `(?i)mount.*not available` | Mount point missing |
| `(?i)path.*not accessible` | Path not accessible |

**Non-retryable failures (always skipped):**

- Corrupted file / checksum mismatch
- Missing expected tracks / streams
- Resolution or codec mismatch
- Custom format score below cutoff (already handled by §3.3)

**Retry schedule (configurable):**

```
Retry 1: after 5 minutes
Retry 2: after 15 minutes
Retry 3: after 30 minutes
Retry 4: after 60 minutes
Retry 5: after 120 minutes
Retry 6: after 240 minutes (final)
Total: 6 retries over ~8 hours
```

Each retry:
1. Re-checks file existence at expected path.
2. Re-runs parse against Sonarr.
3. Re-attempts manual import.
4. If all retries exhausted, marks permanently failed and triggers notification.

**Persistence:** In-memory only for v1. If the agent restarts, pending retries are lost. Acceptable trade-off until Phase 3+.

---

### 3.7 Intelligent Cleanup

Optional background tasks that clean up residual files.

**Cleanup actions (all configurable, all disabled by default):**

| Action | Description | Safety Check |
|---|---|---|
| `removeEmptyFolders` | Deletes empty directories in download roots | Only if path is inside a configured root; never deletes root; excludes `.partial`, `_unpack` |
| `removeSampleFiles` | Deletes files matching `sample*`, `*-sample.*` | File size < 500 MB |
| `removeNFOFiles` | Deletes `.nfo` files | File size < 10 MB |
| `removeBrokenSymlinks` | Deletes dangling symlinks | `os.Lstat` check before removal |
| `removeTempExtraction` | Deletes `_unpack`, `.unpack`, `extracted_*` dirs | Only if no active queue item references these paths |
| `removePartialUnpack` | Deletes files with `.part`, `.partial` extension | Only if age > 24 h |

**Schedule:** Configurable interval (default: once per hour).

---

### 3.8 Safety Engine

The safety engine is the **central gatekeeper** for every destructive action. No action bypasses it.

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
    Field    string `yaml:"field"`    // e.g. "age", "status", "importState"
    Operator string `yaml:"operator"` // eq, neq, gt, gte, lt, lte, in, matches, exists
    Value    string `yaml:"value"`
}

type Action struct {
    Type   string            `yaml:"type"`   // remove_queue, manual_import, retry, cleanup, blacklist
    Params map[string]string `yaml:"params"`
}
```

#### Evaluation Flow

```
For each issue detected:
  1. Match against applicable rules
  2. For each matching rule:
     a. Evaluate ALL conditions (AND logic; short-circuit on first failure)
     b. If all pass → action approved
  3. Check global constraints (always enforced):
     - Item not already in an active action
     - Cooldown: at least 30 min since last action on same series/episode pair
     - Sonarr connectivity confirmed
     - No duplicate action in last 5 min
  4. Execute action (or log if dry-run)
  5. Log full decision with:
     - Item identifier
     - Rule(s) triggered
     - Each condition's result (pass/fail with actual vs expected)
     - Action taken (or reason skipped)
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
    {"field": "age_hours", "operator": "gte", "expected": "2", "actual": "6.3", "passed": true},
    {"field": "import_decision", "operator": "eq", "expected": "not_custom_format_upgrade", "actual": "not_custom_format_upgrade", "passed": true}
  ],
  "action": "remove_from_queue",
  "executed": true,
  "dry_run": false
}
```

---

### 3.9 Dry Run Mode

Global flag (`dryRun: true`) that disables all mutating API calls.

**Behavior when enabled:**

- All monitors run normally.
- All rules evaluate normally.
- All decisions are logged as if the action would occur.
- **No** `POST`/`DELETE` requests are sent to Sonarr.
- Log entries tagged with `"dry_run": true`.
- Dashboard shows "Would have" actions in a distinct style.
- Activity feed includes simulated entries for would-be actions.

**Purpose:** Deploy the agent, observe its behavior, tune rules, build confidence before enabling automation.

---

### 3.10 Dashboard

A minimal embedded web server serving a single-page application.

**Sections:**

| Section | Content |
|---|---|
| **Status Bar** | Connection status, uptime, dry-run indicator, version |
| **Statistics Cards** | Recovered imports, downloads removed, retries performed, pending review count |
| **Current Queue** | Table of current queue items with status, age, identified issues |
| **Pending Review** | List of `ImportSuggestion` items with approve/reject/ignore buttons; confidence breakdown visible |
| **Recent Activity** | Reverse-chronological feed of all decisions (executed or would-have) |
| **Configuration Summary** | Read-only display of active configuration (API key masked) |

**Technical implementation:**

- Go `embed.FS` for static assets (single HTML page, vanilla JS, minimal CSS).
- REST API under `/api/` prefix serves all data.
- No external build step, no npm, no SPA framework.
- Auto-refresh via short-polling (5 second interval).
- Optional auth via configurable token (`Authorization: Bearer <token>`).

---

### 3.11 Notifications

Pluggable notification backends.

**Supported integrations (phased):**

| Integration | Phase | Configuration |
|---|---|---|
| Discord Webhook | Phase 2 | `notifications.discordWebhook` URL |
| Slack Webhook | Phase 2 | `notifications.slackWebhook` URL |
| Gotify | Phase 2 | `notifications.gotify` URL + token |
| ntfy | Phase 2 | `notifications.ntfy` URL + topic |
| Generic Webhook | Phase 2 | `notifications.webhook` URL + method + headers + body template |
| Email (SMTP) | Phase 3 | `notifications.email` SMTP settings |

**Notification events:**

| Event | Default Channels | Purpose |
|---|---|---|
| `import.recovered` | Discord, Slack | Successful automatic manual import |
| `download.removed` | Discord, Slack | Download removed by rule |
| `import.failed_all_retries` | Discord, Gotify | All retries exhausted |
| `manual_review.pending` | Discord | New import suggestion needs review |
| `cleanup.performed` | (none) | Cleanup action executed |
| `error.sonarr_unreachable` | Gotify, ntfy | Sonarr connectivity lost |

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
| `GET /health` | Returns `200 OK` if agent is running |
| `GET /health/sonarr` | Returns `200 OK` if Sonarr is reachable and authenticated |

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

Components communicate via Go channels and shared in-memory state:

- **Monitors** push detected items into per-type channels.
- **Detectors** consume from monitor channels and produce `Issue` values.
- **Safety Engine** receives `Issue` values, evaluates rules, and produces `Decision` values.
- **Action Executor** receives approved `Decision` values and performs Sonarr API calls (or logs as dry-run).
- **Retry Scheduler** manages a timer-based in-memory retry queue.
- **Dashboard API** exposes shared aggregations (last N decisions, current stats, pending suggestions).
- **Cleanup Engine** uses shared volume mount for filesystem access; never calls external APIs for cleanup.

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
│   │   ├── client.go            # Sonarr REST API client
│   │   ├── queue.go             # Queue endpoint calls
│   │   ├── history.go           # History endpoint calls
│   │   ├── manual_import.go     # Manual import calls
│   │   ├── parse.go             # Parse endpoint calls
│   │   ├── system.go            # System status & health
│   │   ├── quality.go           # Quality definitions (fetched at startup)
│   │   └── language.go          # Language definitions (fetched at startup)
│   ├── monitors/
│   │   ├── queue_monitor.go     # Queue polling & diffing
│   │   ├── history_monitor.go   # History polling & diffing
│   │   └── health_monitor.go    # Sonarr connectivity
│   ├── detectors/
│   │   ├── detector.go          # Detector interface
│   │   ├── stuck_download.go    # Stuck download detection
│   │   ├── not_custom_format.go # "Not custom format upgrade" detection
│   │   ├── import_recovery.go   # Failed import recovery detection
│   │   └── cleanup.go           # Cleanup candidate detection
│   ├── recovery/
│   │   ├── import.go            # File scanning, parsing, confidence scoring, manual import
│   │   └── scanner.go           # Directory walking & file matching
│   ├── safety/
│   │   ├── engine.go            # Rule evaluation engine
│   │   ├── rule.go              # Rule & condition types
│   │   └── builtins.go          # Built-in rules derived from config
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
│   │   ├── notifier.go          # Notifier interface
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
│       └── types.go             # Domain types (Issue, Decision, Suggestion, etc.)
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
}

// Core methods:
func (c *Client) GetQueue(ctx context.Context) ([]QueueItem, error)
func (c *Client) GetQueueDetails(ctx context.Context) ([]QueueDetailItem, error)
func (c *Client) GetHistory(ctx context.Context, params HistoryParams) ([]HistoryItem, error)
func (c *Client) GetSystemStatus(ctx context.Context) (SystemStatus, error)
func (c *Client) RemoveQueueItem(ctx context.Context, id int, blocklist bool) error
func (c *Client) Parse(ctx context.Context, path string) (*ParseResult, error)
func (c *Client) ManualImport(ctx context.Context, req ManualImportRequest) error
func (c *Client) GetQualityDefinitions(ctx context.Context) ([]QualityDefinition, error)
func (c *Client) GetLanguages(ctx context.Context) ([]Language, error)
```

**Error handling:**
- All calls use `context.Context` for cancellation and timeouts.
- 4xx (except 429) are terminal errors for that item.
- 5xx and network errors trigger exponential backoff with jitter (max 3 retries).
- Concurrent request limiting via semaphore (max 5 concurrent Sonarr requests).

### 5.2 Queue Monitor (`internal/monitors/queue_monitor.go`)

```go
type QueueMonitor struct {
    client   *sonarr.Client
    interval time.Duration
    issues   chan<- Issue
    lastSeen map[int]QueueState // diff tracking for state transitions
}

// Each tick: fetch queue + details, diff against lastSeen, emit Issues.
```

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

### 5.4 Safety Engine (`internal/safety/engine.go`)

```go
type Engine struct {
    rules       []SafetyRule
    activeItems map[string]time.Time   // active actions tracker
    lastAction  map[string]time.Time   // cooldown tracker (key: "series:episode")
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

**Evaluation order:**
1. Check rule enabled.
2. Check all conditions (AND logic; short-circuit on first failure).
3. Check global constraints.
4. If all pass → `Approved = true`.
5. If `DryRun == true` → `Approved = true` but `Decision.DryRun = true`.
6. Log full decision with all condition results.

### 5.5 Action Executor (`internal/executor/executor.go`)

```go
type Executor struct {
    sonarrClient *sonarr.Client
    notifier     *notifications.Notifier
    dryRun       bool
}

func (e *Executor) Execute(ctx context.Context, decision Decision) error
```

Dispatches to specific handlers. Each handler:
1. Checks `dryRun` — if true, logs and returns nil.
2. Performs the Sonarr API call(s) or filesystem operation.
3. Logs the outcome.
4. Fires notification if configured.

---

## 6. Data Models

### Core Domain Types

```go
// QueueItem represents a Sonarr queue item (simplified from API response).
type QueueItem struct {
    ID                  int             `json:"id"`
    SeriesID            int             `json:"seriesId"`
    EpisodeID           int             `json:"episodeId"`
    SeriesTitle         string          `json:"seriesTitle"`
    EpisodeTitle        string          `json:"episodeTitle"`
    Quality             string          `json:"quality"`
    Size                int64           `json:"size"`
    Status              string          `json:"status"`              // queued, paused, downloading, completed, warning, failed
    TrackedDownloadStatus string        `json:"trackedDownloadStatus"` // ok, warning, error
    TrackedDownloadState   string       `json:"trackedDownloadState"` // downloading, importPending, importing, imported, importFailed, downloadFailed
    StatusMessages      []StatusMessage `json:"statusMessages"`
    ErrorMessage        string          `json:"errorMessage"`
    DownloadID          string          `json:"downloadId"`
    OutputPath          string          `json:"outputPath"`          // download directory path
    Added               time.Time       `json:"added"`
}

type StatusMessage struct {
    Title    string   `json:"title"`
    Messages []string `json:"messages"`
}

// QueueDetailItem adds per-episode detail to a queue item.
type QueueDetailItem struct {
    QueueItem
    Episode *EpisodeResource `json:"episode,omitempty"`
}

type EpisodeResource struct {
    ID             int    `json:"id"`
    SeriesID       int    `json:"seriesId"`
    SeasonNumber   int    `json:"seasonNumber"`
    EpisodeNumber  int    `json:"episodeNumber"`
    Title          string `json:"title"`
}

// HistoryItem represents a Sonarr history record.
type HistoryItem struct {
    ID         int               `json:"id"`
    SeriesID   int               `json:"seriesId"`
    EpisodeID  int               `json:"episodeId"`
    SourceTitle string           `json:"sourceTitle"`
    EventType  string            `json:"eventType"`
    Quality    string            `json:"quality"`
    Date       time.Time         `json:"date"`
    Data       map[string]string `json:"data"`
}

// HistoryParams for filtering history queries.
type HistoryParams struct {
    Page          int    `json:"page"`
    PageSize      int    `json:"pageSize"`
    SortKey       string `json:"sortKey"`
    SortDirection string `json:"sortDirection"`
    EventType     int    `json:"eventType,omitempty"` // 1=grabbed, 3=downloadFolderImported, 4=downloadFailedImport, 7=downloadIgnored
    SeriesID      int    `json:"seriesId,omitempty"`
    EpisodeID     int    `json:"episodeId,omitempty"`
}

// ManualImportRequest sent to Sonarr's manual import endpoint.
type ManualImportRequest struct {
    Path         string        `json:"path"`
    SeriesID     int           `json:"seriesId"`
    SeasonNumber int           `json:"seasonNumber"`
    EpisodeIDs   []int         `json:"episodeIds"`
    Quality      QualityModel  `json:"quality"`
    Language     LanguageModel `json:"language"`
    DownloadID   string        `json:"downloadId"`
    ImportMode   string        `json:"importMode"` // "move" or "copy"
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

// ParseResult returned by Sonarr's parse endpoint.
type ParseResult struct {
    Title             string             `json:"title"`
    ParsedEpisodeInfo *ParsedEpisodeInfo `json:"parsedEpisodeInfo"`
    Series            *SeriesInfo        `json:"series"`
    Episodes          []EpisodeLookup    `json:"episodes"`
}

type ParsedEpisodeInfo struct {
    ReleaseTitle             string       `json:"releaseTitle"`
    SeriesTitle              string       `json:"seriesTitle"`
    SeasonNumber             int          `json:"seasonNumber"`
    EpisodeNumbers           []int        `json:"episodeNumbers"`
    AbsoluteEpisodeNumbers   []int        `json:"absoluteEpisodeNumbers"`
    FullSeason               bool         `json:"fullSeason"`
    Quality                  QualityModel `json:"quality"`
    Language                 LanguageModel `json:"language"`
}

type SeriesInfo struct {
    Title     string `json:"title"`
    TVDBID    int    `json:"tvdbId"`
    ImdbID    string `json:"imdbId"`
}

type EpisodeLookup struct {
    ID            int    `json:"id"`
    EpisodeNumber int    `json:"episodeNumber"`
    SeasonNumber  int    `json:"seasonNumber"`
    Title         string `json:"title"`
}

// QualityDefinition fetched from Sonarr at startup.
type QualityDefinition struct {
    ID     int    `json:"id"`
    Name   string `json:"name"`
    Title  string `json:"title"`
}

// Language fetched from Sonarr at startup.
type Language struct {
    ID   int    `json:"id"`
    Name string `json:"name"`
}

// SystemStatus returned by Sonarr's system status endpoint.
type SystemStatus struct {
    Version string `json:"version"`
}
```

---

## 7. Rule Engine

### Built-in Rules (Derived from Config)

Each configurable automation setting generates internal safety rules at startup.

**Example — config:**

```yaml
automation:
  removeNotCustomFormat:
    enabled: true
    waitHours: 2
```

**Generates rule:**

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

### Global Safety Constraints (Always Enforced)

1. **No duplicate actions**: Same item, same rule, within 5 minutes → skip.
2. **Cooldown period**: At least 30 minutes between actions on same series/episode pair.
3. **Sonarr connectivity required**: Last health check must have succeeded.
4. **Atomicity**: If first step of multi-step action fails, do not proceed.

---

## 8. Configuration Schema

```yaml
# Sonarr Recovery Agent Configuration
# Version: 2.0.0

# ─── Sonarr Connection ───
sonarr:
  url: http://sonarr:8989          # REQUIRED. Sonarr base URL.
  apiKey: ""                        # REQUIRED. Sonarr API key.
  timeout: 30s                      # HTTP request timeout.
  maxConcurrency: 5                 # Max concurrent Sonarr API requests.

# ─── Monitoring ───
monitoring:
  queueInterval: 30s                # Queue polling interval.
  historyInterval: 5m              # History polling interval.
  healthInterval: 60s              # Sonarr health check interval.
  startupDelay: 10s                # Delay before first poll cycle.

# ─── File System ───
paths:
  downloadRoots: []                 # Override: explicit download roots for scanning.
                                    # If empty, inferred from Sonarr queue item outputPaths.
                                    # Must match volume mounts in container.

# ─── Automation ───
automation:

  # ── Remove "Not a Custom Format Upgrade" downloads ──
  removeNotCustomFormat:
    enabled: true
    waitHours: 2                    # Minimum age before removal.
    blocklistRelease: false         # Also blocklist the release.
    statusMessageRegex: ""          # Custom regex (defaults to built-in).

  # ── Remove broken/stuck downloads ──
  removeBrokenDownloads:
    enabled: true
    waitHours: 6                    # Minimum age before removal.
    blocklistRelease: false
    errorConditions:                # Which error states trigger removal.
      - missing_files
      - abandoned

  # ── Retry failed imports ──
  retryImports:
    enabled: true
    retryIntervals:                 # Retry schedule.
      - 5m
      - 15m
      - 30m
      - 1h
      - 2h
      - 4h
    retryableErrors:                # Error patterns that trigger retry.
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

  # ── Automatic manual import ──
  autoManualImport:
    enabled: false                  # DEFAULT OFF. Enable after dry-run testing.
    minimumConfidence: 95           # Auto-import if confidence >= this.
    manualReviewThreshold: 70       # Create review suggestion if confidence >= this.

  # ── Intelligent cleanup ──
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
    bodyTemplate: ""
  email:
    enabled: false
    smtpHost: ""
    smtpPort: 587
    smtpUsername: ""
    smtpPassword: ""
    from: ""
    to: []

  events:
    import_recovered: [discord]
    download_removed: [discord]
    import_failed_all_retries: [discord, gotify]
    manual_review_pending: [discord]
    cleanup_performed: []

# ─── Logging ───
logging:
  level: info                       # debug, info, warn, error.
  format: json                      # json or text.

# ─── Global ───
dryRun: true                        # DEFAULT TRUE. Set to false to enable actions.
```

### Environment Variable Overrides

All config keys can be overridden via environment variables using the prefix `SRA_` and double-underscore separators:

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

On startup, the agent must validate and fail fast with clear errors for:

| Check | Rule |
|---|---|
| `sonarr.url` | Must be a valid HTTP(S) URL |
| `sonarr.apiKey` | Must be non-empty |
| `sonarr.timeout` | Must be > 0 |
| All duration values | Must parse as valid Go durations |
| `autoManualImport.minimumConfidence` | Must be 0-100 |
| `autoManualImport.manualReviewThreshold` | Must be 0-100 and <= `minimumConfidence` |
| `retryImports.retryIntervals` | Must be non-empty if `retryImports.enabled` |
| `notifications.email.*` | If `notifications.email.enabled`, all SMTP fields required |
| `paths.downloadRoots` | If provided, each path must exist and be readable |
| Config file itself | Must parse as valid YAML with no unknown top-level keys |

---

## 9. API Design

### Internal REST API (Dashboard Backend)

Base path: `/api/v1/`

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/status` | Agent status (version, uptime, dry-run, sonarr status) |
| `GET` | `/api/v1/stats` | Aggregated statistics (counts of actions, items) |
| `GET` | `/api/v1/queue` | Current Sonarr queue items (cached, from last poll) |
| `GET` | `/api/v1/suggestions` | List pending import suggestions |
| `POST` | `/api/v1/suggestions/{id}/approve` | Approve a suggestion (triggers import) |
| `POST` | `/api/v1/suggestions/{id}/reject` | Reject a suggestion |
| `POST` | `/api/v1/suggestions/{id}/ignore` | Ignore a suggestion for configured duration |
| `GET` | `/api/v1/activity` | Recent decisions feed (limit/offset query params) |
| `GET` | `/api/v1/config` | Read-only config summary (API key masked) |
| `GET` | `/health` | Health check (returns 200) |
| `GET` | `/health/sonarr` | Sonarr connectivity check |
| `GET` | `/metrics` | Prometheus metrics endpoint |

### Authentication

Dashboard endpoints protected by token when `dashboard.authToken` is configured:

```
Authorization: Bearer <authToken>
```

`/health*` and `/metrics` endpoints are not protected.

---

## 10. Dashboard

### Technology

- Single HTML file with embedded CSS and JavaScript.
- Zero external dependencies (no npm, no frameworks, no CDN).
- Served via Go's `embed.FS` — no build step.

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
│  │        Age: 6h  Status: completed           ││
│  ├─────────────────────────────────────────────┤│
│  │ 09:42  Recovered: Breaking Bad S05E14      ││
│  │        Confidence: 98%  (TVDB ✓ Season ✓   ││
│  │         Episode ✓ Quality ✓ Language ✓)    ││
│  ├─────────────────────────────────────────────┤│
│  │ 09:15  Suggestion Created                  ││
│  │        Better.Call.Saul.S06E03.mkv         ││
│  │        Confidence: 85%  [Approve] [Reject] ││
│  │        Breakdown: TVDB ✓ Season ✓ Ep ✗     ││
│  └─────────────────────────────────────────────┘│
└─────────────────────────────────────────────────┘
```

---

## 11. Notifications

### Discord Template (Recovered Import)

```json
{
  "embeds": [{
    "title": "Recovered Import",
    "description": "**Series:** Foundation\n**Episode:** S02E08\n**Confidence:** 98%\n**Breakdown:** TVDB ✓ | Season ✓ | Episode ✓ | Quality ✓ | Language ✓",
    "color": 3066993,
    "timestamp": "2026-07-23T09:42:00Z",
    "footer": {"text": "Sonarr Recovery Agent"}
  }]
}
```

### Slack Template (Recovered Import)

```json
{
  "blocks": [
    {"type": "header", "text": {"type": "plain_text", "text": "Recovered Import"}},
    {"type": "section", "fields": [
      {"type": "mrkdwn", "text": "*Series:*\nFoundation"},
      {"type": "mrkdwn", "text": "*Episode:*\nS02E08"},
      {"type": "mrkdwn", "text": "*Confidence:*\n98%"},
      {"type": "mrkdwn", "text": "*Breakdown:*\nTVDB ✓ Season ✓ Ep ✓ Q ✓ L ✓"}
    ]}
  ]
}
```

---

## 12. Metrics & Observability

All metrics under the `sra_` namespace.

```
# HELP sra_imports_recovered_total Number of successful automatic manual imports.
# TYPE sra_imports_recovered_total counter
sra_imports_recovered_total{confidence_bucket="95-100"} 5

# HELP sra_downloads_removed_total Number of downloads removed by rule.
# TYPE sra_downloads_removed_total counter
sra_downloads_removed_total{reason="not_custom_format"} 14
sra_downloads_removed_total{reason="stuck_download"} 3

# HELP sra_retries_total Number of import retries attempted.
# TYPE sra_retries_total counter
sra_retries_total{outcome="success"} 2
sra_retries_total{outcome="failed"} 1

# HELP sra_decisions_evaluated_total Number of safety rule evaluations.
# TYPE sra_decisions_evaluated_total counter
sra_decisions_evaluated_total{rule="remove_not_custom_format",passed="true"} 42
sra_decisions_evaluated_total{rule="remove_not_custom_format",passed="false"} 12

# HELP sra_queue_items_observed Current number of queue items observed.
# TYPE sra_queue_items_observed gauge
sra_queue_items_observed 18

# HELP sra_suggestions_pending Number of import suggestions awaiting review.
# TYPE sra_suggestions_pending gauge
sra_suggestions_pending 2

# HELP sra_sonarr_up Whether Sonarr is reachable (1) or not (0).
# TYPE sra_sonarr_up gauge
sra_sonarr_up 1

# HELP sra_cycle_duration_seconds Duration of each monitoring cycle.
# TYPE sra_cycle_duration_seconds histogram
sra_cycle_duration_seconds_bucket{monitor="queue",le="0.5"} 100
```

---

## 13. Testing Strategy

### Unit Tests

| Layer | Coverage Target | Focus |
|---|---|---|
| `internal/safety/` | 95%+ | Rule evaluation, condition logic, edge cases |
| `internal/recovery/` | 90%+ | Confidence scoring, file matching, parse interpretation |
| `internal/config/` | 90%+ | Config loading, env overrides, validation |
| `internal/detectors/` | 85%+ | Issue detection with mocked Sonarr data |
| `internal/executor/` | 85%+ | Action dispatch, dry-run, error handling |
| `internal/notifications/` | 80%+ | Template rendering, webhook payload construction |

### Integration Tests

**Sonarr API Mock:** `httptest.Server` simulating Sonarr API responses (queue, history, parse, manual import, quality, language).

**End-to-end scenarios:**
1. Stuck download → safety check passes → removal (or dry-run skip).
2. "Not a Custom Format Upgrade" → wait time satisfied → removal.
3. Failed import → recovery scans directory → high confidence → auto-import with multi-episode support.
4. Failed import → medium confidence → suggestion created with confidence breakdown.
5. Transient error → retry schedule → recovery on retry N.
6. Dry-run enabled → all detections work, zero mutations.

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
      - ./data:/data:ro              # Read-only for file scanning
      - ./remediator-config:/config
    environment:
      - SRA_SONARR__URL=http://sonarr:8989
      - SRA_SONARR__API_KEY=${SONARR_API_KEY}
      - SRA_DRY_RUN=true
    ports:
      - "8080:8080"
    depends_on:
      - sonarr
```

---

## 15. Phase Roadmap

### Phase 1 — Core Foundation (MVP)

| Feature | Priority |
|---|---|
| Configuration loading (YAML + env) with strict validation | Must |
| Sonarr API client (queue, history, status) | Must |
| Queue & history monitors | Must |
| "Not a Custom Format Upgrade" detector (message + history) | Must |
| Stuck download detector | Must |
| Safety engine with built-in rules | Must |
| Dry-run mode | Must |
| Structured JSON logging | Must |
| Action executor (queue removal via Sonarr API) | Must |
| Docker image & Compose example | Must |

**Deliverable:** Container that monitors Sonarr, detects stuck downloads and "not an upgrade" items, logs what it would do, and removes them (when dry-run disabled).

### Phase 2 — Recovery & Visibility

| Feature | Priority |
|---|---|
| Import recovery engine (scan + parse + match + confidence) | Must |
| Manual import suggestion system with confidence breakdown | Must |
| Manual import execution (auto + manual approval) | Must |
| Multi-episode support in import | Must |
| Retry engine with configurable intervals | Must |
| Quality & language definition fetching at startup | Must |
| Dashboard (status, stats, queue, activity, suggestions) | Must |
| Dashboard auth token | Should |
| Discord, Slack, Gotify, ntfy, webhook notifications | Must |

**Deliverable:** Full recovery agent with dashboard, notifications, and manual review workflow.

### Phase 3 — Polish & Production

| Feature | Priority |
|---|---|
| Intelligent cleanup engine | Should |
| Prometheus metrics endpoint | Must |
| Health endpoints | Must |
| Email notification (SMTP) | Should |
| Config hot-reload | Could |
| Custom user-defined rules | Could |
| Comprehensive integration tests | Must |
| Documentation | Must |

**Deliverable:** Production-ready release with metrics, docs, and full test coverage.

### Phase 4 — Multi-*arr Support

| Feature |
|---|
| Refactor to generic `pkg/arrclient` |
| Radarr adapter |
| Lidarr adapter |
| Readarr adapter |
| Multi-app dashboard |
| Shared rule engine across *arr types |

---

## 16. Long-Term Vision

The project should become a generic **Arr Recovery Agent** capable of autonomously monitoring and maintaining media automation stacks. Support for Sonarr is the initial implementation, but the core architecture should make it straightforward to extend to Radarr, Lidarr, and Readarr by implementing application-specific API adapters.

### Core Design Principles

1. **Safety first, automation second.** Prefer observation and reporting over risky assumptions. Build confidence through dry-run mode.
2. **Transparency.** Every action includes a human-readable explanation. Decision logs show exactly which rules fired and why.
3. **Explainability.** A non-technical user reading the dashboard should understand why the agent did something.
4. **Configurability.** Nothing is hardcoded. Every threshold, pattern, and parameter is configurable. Defaults are safe and conservative.
5. **Modularity.** Reusable components for monitoring, rule evaluation, recovery, cleanup, notifications, metrics, and dashboards.
6. **Reversibility.** Where possible, actions should be reversible. Never perform irreversible destructive actions without explicit user configuration.

---

## 17. Appendix: Sonarr API Surface

The agent uses the Sonarr v3 API (used by both Sonarr v3 and v4 installations). Key endpoints:

### Queue
- `GET /api/v3/queue` — List queue items.
- `GET /api/v3/queue/details` — Queue items with episode details.
- `DELETE /api/v3/queue/{id}` — Remove item from queue. Query param: `blocklist=true` to also blocklist.

### History
- `GET /api/v3/history` — History of all events.
  - Params: `page`, `pageSize`, `sortKey`, `sortDirection`, `eventType`, `episodeId`, `seriesId`

### Manual Import
- `GET /api/v3/manualimport` — Files available for manual import.
- `POST /api/v3/manualimport` — Execute manual import.

### Parse
- `GET /api/v3/parse` — Parse a file path or title. Query: `path` or `title`.

### System
- `GET /api/v3/system/status` — System status (version, health).

### Quality & Language (Fetched at Startup)
- `GET /api/v3/qualitydefinition` — Quality definitions.
- `GET /api/v3/language` — Language profiles.

### Episode / Series (Reference)
- `GET /api/v3/episode/{id}` — Episode details.
- `GET /api/v3/series/{id}` — Series details.
- `GET /api/v3/series` — List all series.

---

*End of specification.*
