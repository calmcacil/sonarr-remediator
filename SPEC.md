# Sonarr Recovery Agent — Project Specification

**Version:** 1.0.0
**Status:** Draft
**Repository:** sonarr-remediator

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

Sonarr Recovery Agent is a lightweight Go microservice that runs alongside Sonarr as a sidecar Docker container. Its purpose is to autonomously detect, analyze, and recover from common download and import issues that normally require manual intervention.

It is **not** a cleanup script. It is a continuous recovery agent that observes Sonarr's state, evaluates configurable safety rules, and acts only when it is confident an action is safe.

### Key Characteristics

| Property | Value |
|---|---|
| Language | Go (1.26+) |
| Runtime | Docker container |
| State | Stateless (read-only observation of Sonarr; no local persistence required) |
| Configuration | YAML file + environment variable overrides |
| Observability | Structured JSON logging, Prometheus metrics, health endpoints |
| Safety | Dry-run mode, rule engine gate on every destructive action, human-readable decision logs |

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

- Managing download clients (only unmonitoring/deleting downloads that Sonarr has already processed).
- Replacing Sonarr's internal download decision logic.
- Acting as a media manager or indexer.
- Multi-instance coordination (single Sonarr per agent instance).
- Historical analytics beyond Prometheus metrics.
- Full CRUD management UI for Sonarr (this is a recovery agent, not a Sonarr client).

---

## 3. Core Features

### 3.1 Queue Monitoring

Continuously polls and evaluates every item in the Sonarr download queue and history.

**Data sources polled:**

| Endpoint | Purpose | Interval |
|---|---|---|
| `/api/v3/queue` | Active and queued downloads (including warnings, errors, status) | 30 s |
| `/api/v3/queue/details` | Per-episode granular detail (import failures, status messages) | 30 s |
| `/api/v3/history` | Completed/failed import history | 5 min |
| `/api/v3/system/status` | Sonarr health and connectivity | 60 s |

**Tracked states per queue item:**

- Queued / downloading / completed / warning / failed (download client)
- Import pending / importing / imported / import failed
- Stalled by error messages ("No files found are eligible for import", "Not a Custom Format Upgrade", etc.)
- Duration in current state (age tracking)

---

### 3.2 Automatic Cleanup of Stuck Downloads

Detects downloads that have become permanently stuck and will never import successfully.

**Trigger conditions (any single one is sufficient):**

| Condition | Detection Method |
|---|---|
| Download client reports error | Queue item has `errorMessage` set and state is `warning` |
| Missing files | Queue item `status` is `warning`, message contains "No files found" |
| Corrupted download | Download client reports corruption; Sonarr import fails with CRC/hash error |
| Sonarr abandoned item | Queue item is in `completed` but history shows no import attempt after configurable timeout (default 6 h) |
| Configurable age timeout exceeded | `Time since download completed > maxAge` AND no import in progress |
| Import repeatedly failed | History shows N consecutive failed import attempts for same episode |

**Configurable actions (ordered, all optional):**

1. Remove from Sonarr queue (`DELETE /api/v3/queue/{id}`)
2. Remove from download client (`DELETE /api/v3/queue/{id}?removeFromClient=true`)
3. Blacklist release (`POST /api/v3/blacklist` with reason)
4. Delete files from disk (`DELETE /api/v3/queue/{id}?removeFromClient=true&blocklist=true`)
5. Log only (no action)

**Required safety gates:**

- Item age ≥ configurable minimum (default 2 h)
- Not currently in "importing" state
- No manual import scheduled for this item
- Dry-run check
- Rule enabled check

---

### 3.3 "Not a Custom Format Upgrade" Removal

One of the most common and annoying Sonarr patterns: a download completes, Sonarr determines it is not an upgrade over the existing file (based on custom format scoring), but leaves the completed download sitting in the queue and in the download client.

**Detection:**

- Queue item is in `completed` status (download finished)
- Queue item's tracked download status contains message: `"Not a Custom Format Upgrade"`
- Or history item's `eventType` is `downloadIgnored` / `downloadFailedImport` with import message matching `"Not an upgrade"`

**Safety gates:**

| Condition | Configurable |
|---|---|
| Download completed | Implicit from status |
| Import decision confirmed as no-upgrade | Static message check + configurable regex |
| Age ≥ waitHours (default 2) | `automation.removeNotCustomFormat.waitHours` |
| Not currently importing | Always enforced |
| No active retry scheduled | Always enforced |
| No other active queue item for same episode | Always enforced |
| Rule enabled | `automation.removeNotCustomFormat.enabled` |

**Default action:**
Remove from queue + remove from download client. File deletion is optional (`deleteFiles: true|false`).

---

### 3.4 Import Recovery

For downloads where the download client succeeded but Sonarr could not automatically match and import the file(s).

**Common failure causes:**

- Strange or obfuscated release names
- Download client renamed the folder
- Multi-file releases (CD1, CD2; sample + main; extras)
- Anime absolute numbering vs season/episode
- Scene naming conventions Sonarr cannot parse
- Folder name / file name mismatch
- Unpacked output in unexpected subdirectories

**Recovery workflow:**

```
1. DETECT
   Queue item: download completed, import failed.
   History shows: eventType=downloadFailedImport.

2. LOCATE FILES
   For each failed import, scan the download directory for candidate video files.
   Extensions: .mkv, .mp4, .avi, .m4v, .mov, .wmv, .ts, .iso
   Exclude: sample directories, extras directories, nfo/txt/jpg/png files.

3. PARSE
   Send each candidate file path to Sonarr's parse endpoint:
   GET /api/v3/parse?path=/absolute/path/to/file&title=ReleaseName

   Sonarr returns parsed metadata: series title, season number, episode numbers,
   quality, language, and a confidence indicator.

4. MATCH
   Cross-reference parsed result with the expected series/episode from the
   original queue item. Verify:
   - Parsed series TVDB/TVDB ID matches expected
   - Parsed season matches expected
   - Parsed episode(s) match expected

5. EVALUATE CONFIDENCE
   Compute a confidence score (0-100) based on:
   - Sonarr parse success (did it return a valid parse?)
   - TVDB ID match (+40)
   - Season match (+30)
   - Episode match (+30)
   - Parse quality/edition recognized (+10)
   - Manual weighting adjustments

6. IMPORT
   POST /api/v3/manualimport
   {
     "path": "/path/to/file.mkv",
     "seriesId": 123,
     "seasonNumber": 5,
     "episodeIds": [14],
     "quality": { "quality": { "id": 7 }, "revision": { "version": 1 } },
     "language": { "id": 1 },
     "downloadId": "original-download-id",
     "importMode": "move"
   }

7. LOG + NOTIFY
   Record action with full explanation.
```

**Auto-import threshold:**

- If `confidence >= autoManualImport.minimumConfidence` (default 95), perform import automatically.
- If `confidence < threshold` but `>= manualReview.minimumConfidence` (default 70), create a dashboard suggestion for manual review.
- If `confidence < manualReview.minimumConfidence`, take no action (log only).

---

### 3.5 Manual Import Assistant

When automatic recovery is not confident enough, the system generates import suggestions for manual review.

**Suggestion model:**

```go
type ImportSuggestion struct {
    ID            string    `json:"id"`
    FilePath      string    `json:"filePath"`
    FileSize      int64     `json:"fileSize"`
    SeriesTitle   string    `json:"seriesTitle"`
    SeasonNumber  int       `json:"seasonNumber"`
    EpisodeNumber int       `json:"episodeNumber"`
    Confidence    int       `json:"confidence"` // 0-100
    MatchDetails  string    `json:"matchDetails"` // human-readable explanation
    CreatedAt     time.Time `json:"createdAt"`
    Status        string    `json:"status"` // pending, approved, rejected
}
```

**Dashboard actions for suggestions:**

- **Approve**: Performs the manual import immediately.
- **Reject**: Dismisses the suggestion (optionally deletes files / removes download).
- **Ignore**: Keeps suggestion in list but marks as ignored for N hours.
- **Auto-approve threshold override**: User can adjust minimum confidence for auto-approval per series or globally.

**Approval workflow:**

```
User clicks "Approve"
  → System runs full safety re-check (conditions may have changed)
  → If still valid, performs POST /api/v3/manualimport
  → Updates suggestion status to "approved"
  → Logs & notifies
```

---

### 3.6 Retry Failed Imports

Re-queues imports that failed due to transient conditions.

**Retryable failure signatures (regex-matched against error messages):**

| Pattern | Reason |
|---|---|
| `Permission denied`, `Access denied` | NAS/FS permission issue |
| `No such file or directory` | NAS/SMB temporarily unavailable |
| `Connection refused`, `Connection timed out` | NFS mount down |
| `No space left on device` | Disk full |
| `Input/output error` | Transient I/O |
| `Destination.*locked`, `File.*in use` | File locked by another process |
| `mount.*not available`, `path.*not accessible` | Mount point missing |

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
1. Re-verifies the file still exists at the expected path.
2. Re-checks Sonarr's parse result (may change if NAS comes back).
3. Re-attempts manual import via the same `POST /api/v3/manualimport` endpoint.
4. If all retries exhausted, marks as permanently failed and triggers notification.

---

### 3.7 Intelligent Cleanup

Optional background tasks that clean up residual files.

**Cleanup actions (all configurable, all disabled by default):**

| Action | Description | Safety Check |
|---|---|---|
| `removeEmptyFolders` | Deletes empty directories in download root | Recursive; never deletes if path is a root path; excludes paths with `.partial` or `_unpack` |
| `removeSampleFiles` | Deletes files matching `sample*`, `*-sample.*` | File size < 500 MB |
| `removeNFOFiles` | Deletes `.nfo` files | File size < 10 MB |
| `removeBrokenSymlinks` | Deletes dangling symlinks | Checks `os.Lstat` first |
| `removeTempExtraction` | Deletes `_unpack`, `.unpack`, `extracted_*` dirs | Only if no active queue items reference these paths |
| `removePartialUnpack` | Deletes files with `.part`, `.partial` extension | Only if age > 24 h (download client long gone) |

**Schedule:** Configurable interval (default: once per hour, off-peak window optional).

---

### 3.8 Safety Engine

The safety engine is the **central gatekeeper** for every destructive action the agent can perform. No action bypasses it.

#### Rule Model

```go
type SafetyRule struct {
    ID          string   `yaml:"id"`
    Description string   `yaml:"description"`
    Conditions  []Condition `yaml:"conditions"`
    Action      Action   `yaml:"action"`
    Enabled     bool     `yaml:"enabled"`
}

type Condition struct {
    Field    string `yaml:"field"`    // e.g. "age", "status", "importState"
    Operator string `yaml:"operator"` // eq, neq, gt, gte, lt, lte, in, matches, exists
    Value    string `yaml:"value"`
}

type Action struct {
    Type    string            `yaml:"type"`    // remove_queue, remove_client, delete_files, manual_import, retry, cleanup
    Params  map[string]string `yaml:"params"`
}
```

#### Evaluation Flow

```
For each item found by a monitor:
  1. Match against rule triggers (which rules apply to this item?)
  2. For each matching rule:
     a. Evaluate ALL conditions
     b. Short-circuit on first failure
     c. If all pass → action is approved
  3. Check global safety constraints (always enforced, non-configurable):
     - dryRun == false (unless explicitly allowed for testing)
     - Item not already in an active action
     - No circular dependency (same item already had this action this cycle)
  4. Execute action
  5. Log decision with:
     - Item identifier
     - Rule(s) that triggered
     - Conditions evaluated (pass/fail for each)
     - Final action taken (or reason for skip)
```

#### Decision Log Format

```json
{
  "timestamp": "2026-07-23T10:12:00Z",
  "decision_id": "dec_abc123",
  "item": {
    "type": "queue_item",
    "id": "420",
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
  "action": "remove_from_queue_and_client",
  "executed": true,
  "dry_run": false
}
```

---

### 3.9 Dry Run Mode

Global flag (`dryRun: true`) that disables all mutating actions.

**Behavior when dry-run is enabled:**

- All monitors run normally.
- All rules are evaluated normally.
- All decisions are logged as if the action would occur.
- **No** write requests are sent to Sonarr or the download client.
- Log entries are tagged with `"dry_run": true`.
- Dashboard shows "Would have taken" actions in a distinct style/color.

**Purpose:** Allow users to deploy the agent, observe its behavior, tune rules and thresholds, and build confidence over days or weeks before enabling automation.

---

### 3.10 Dashboard

A minimal embedded web server serving a single-page application.

**Pages / Sections:**

| Section | Content |
|---|---|
| **Status Bar** | Connection status, uptime, dry-run indicator, version |
| **Statistics Cards** | Recovered imports (count), Removed downloads (count), Retried imports (count), Pending review (count) |
| **Current Queue** | Table of current queue items with status, age, and any identified issues |
| **Pending Review** | List of `ImportSuggestion` items with approve/reject/ignore buttons |
| **Recent Activity** | Reverse-chronological feed of all decisions (actions taken or would-have-taken) |
| **Configuration Summary** | Read-only display of active configuration (masked API key) |

**Technical implementation:**

- Go `embed.FS` for static assets (single HTML page, minimal vanilla JS, minimal CSS).
- REST API under `/api/` prefix serves all data.
- No external build step, no npm, no SPA framework.
- Auto-refresh via short-polling (5 second interval).
- Basic authentication via a configurable dashboard token (simple `Authorization: Bearer <token>`).

---

### 3.11 Notifications

Pluggable notification backend.

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

| Event | Default Channels | Template |
|---|---|---|
| `import.recovered` | Discord, Slack | Recovered Import: {series} - {episode} (confidence: {confidence}%) |
| `download.removed` | Discord, Slack | Removed Download: {title} (reason: {reason}) |
| `import.failed_all_retries` | Discord, Gotify | Import Failed After All Retries: {title} |
| `manual_review.pending` | Discord | New Import Suggestion Needs Review: {series} - {episode} |
| `cleanup.performed` | (none by default) | Cleaned up: {count} files/dirs |
| `error.sonarr_unreachable` | Gotify, ntfy | Sonarr Unreachable: {error} |

---

### 3.12 Metrics & Observability

**Prometheus metrics endpoint:** `GET /metrics`

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `sonarr_recovery_imports_recovered_total` | Counter | `confidence_bucket` | Successful automatic manual imports |
| `sonarr_recovery_downloads_removed_total` | Counter | `reason` | Downloads removed by rule |
| `sonarr_recovery_retries_total` | Counter | `outcome` (success, failed) | Import retries attempted |
| `sonarr_recovery_cleanup_actions_total` | Counter | `action` | Cleanup actions performed |
| `sonarr_recovery_decisions_evaluated_total` | Counter | `rule`, `passed` | Safety rule evaluations |
| `sonarr_recovery_rules_passed_total` | Counter | `rule` | Rules that passed all conditions |
| `sonarr_recovery_queue_items_observed` | Gauge | — | Current count of queue items |
| `sonarr_recovery_suggestions_pending` | Gauge | — | Count of import suggestions awaiting review |
| `sonarr_recovery_sonarr_up` | Gauge | — | 1 if Sonarr is reachable, 0 if not |
| `sonarr_recovery_cycle_duration_seconds` | Histogram | `monitor` | Duration of each monitoring cycle |

**Health endpoints:**

| Endpoint | Purpose |
|---|---|
| `GET /health` | Returns `200 OK` if agent is running |
| `GET /health/sonarr` | Returns `200 OK` if Sonarr is reachable and authenticated |

**Logging:**

- Structured JSON logs to stdout (Docker-native).
- Log levels: `debug`, `info`, `warn`, `error`.
- Log fields: `timestamp`, `level`, `component`, `message`, `item` (object), `rule` (string), `action` (string), `dry_run` (bool), `error` (string if applicable).

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
        +-------+--------+
        |                 |
  Download Client   Web Dashboard
  (external)        (browser)
```

### Internal Component Communication

Components communicate via Go channels and shared in-memory state:

- **Monitors** push detected items into per-type channels (e.g., `chan QueueItem`, `chan HistoryItem`).
- **Detectors** consume from monitor channels and produce `Issue` values.
- **Safety Engine** receives `Issue` values, evaluates rules, and produces `Decision` values.
- **Action Executor** receives approved `Decision` values and performs the action (or logs it as dry-run).
- **Retry Scheduler** manages a timer-based in-memory retry queue.
- **Dashboard API** reads from shared aggregations (last N decisions, current stats, pending suggestions).

### Directory Structure

```
sonarr-remediator/
├── cmd/
│   └── sonarr-remediator/
│       └── main.go              # Entry point, config loading, service wiring
├── internal/
│   ├── config/
│   │   ├── config.go            # Configuration struct & loading (YAML + env)
│   │   └── defaults.go          # Default configuration values
│   ├── sonarr/
│   │   ├── client.go            # Sonarr REST API client
│   │   ├── queue.go             # Queue-related API calls
│   │   ├── history.go           # History-related API calls
│   │   ├── manual_import.go     # Manual import API calls
│   │   ├── parse.go             # Parse API calls
│   │   └── types.go             # Sonarr API type definitions
│   ├── monitors/
│   │   ├── queue_monitor.go     # Queue polling & diffing
│   │   ├── history_monitor.go   # History polling & diffing
│   │   └── health_monitor.go    # Sonarr connectivity monitoring
│   ├── detectors/
│   │   ├── detector.go          # Issue detector interface
│   │   ├── stuck_download.go    # Stuck download detection logic
│   │   ├── not_custom_format.go # "Not custom format upgrade" detection
│   │   ├── import_recovery.go   # Failed import recovery detection
│   │   └── cleanup.go           # Cleanup candidate detection
│   ├── recovery/
│   │   ├── manual_import.go     # File scanning, parsing, confidence scoring
│   │   └── path_scanner.go      # Directory walking & file matching
│   ├── safety/
│   │   ├── engine.go            # Rule evaluation engine
│   │   ├── rule.go              # Rule & condition types
│   │   └── builtins.go          # Built-in safety rules (mapped from config)
│   ├── executor/
│   │   ├── executor.go          # Action execution interface
│   │   ├── queue_actions.go     # Queue removal actions
│   │   ├── import_actions.go    # Manual import actions
│   │   ├── cleanup_actions.go   # File/directory cleanup actions
│   │   └── retry.go             # Retry scheduling & execution
│   ├── scheduler/
│   │   └── scheduler.go         # Periodic task scheduler
│   ├── dashboard/
│   │   ├── server.go            # HTTP server & router
│   │   ├── api.go               # Dashboard REST API handlers
│   │   ├── auth.go              # Simple token auth middleware
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
│   │   └── metrics.go           # Prometheus metrics registration & endpoint
│   ├── logging/
│   │   └── logging.go           # Structured logger setup (zerolog or slog)
│   └── types/
│       └── types.go             # Shared domain types (Issue, Decision, Suggestion, etc.)
├── pkg/
│   └── arrclient/               # Future: generic *arr API client (for Radarr, Lidarr, etc.)
├── config.example.yaml
├── Dockerfile
├── docker-compose.example.yaml
├── go.mod
├── go.sum
├── Makefile
├── README.md
└── SPEC.md                      # This file
```

---

## 5. Component Specifications

### 5.1 Sonarr API Client (`internal/sonarr/client.go`)

Wraps all Sonarr REST API calls.

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
func (c *Client) RemoveQueueItem(ctx context.Context, id int, removeFromClient bool, blocklist bool) error
func (c *Client) Parse(ctx context.Context, path, title string) (*ParseResult, error)
func (c *Client) ManualImport(ctx context.Context, req ManualImportRequest) error
func (c *Client) GetManualImportItems(ctx context.Context, params ManualImportParams) ([]ManualImportItem, error)
```

**Error handling:**

- All calls use `context.Context` for cancellation and timeouts.
- 4xx responses (except 429) are logged and treated as terminal errors for that item.
- 5xx and network errors trigger exponential backoff with jitter (max 3 retries for polling calls).
- Concurrent request limiting via a semaphore (max 5 concurrent Sonarr requests).

### 5.2 Queue Monitor (`internal/monitors/queue_monitor.go`)

```go
type QueueMonitor struct {
    client   *sonarr.Client
    interval time.Duration
    issues   chan<- Issue       // output channel
    lastSeen map[int]QueueState // diff tracking
}

// On each tick:
//  1. Fetch queue + queue details
//  2. Diff against lastSeen to detect state transitions
//  3. For each item in problematic state, construct an Issue
//  4. Push Issue to output channel
//  5. Update lastSeen
```

### 5.3 Issue Detector Interface (`internal/detectors/detector.go`)

```go
type Detector interface {
    Name() string
    Detect(ctx context.Context, queueItem QueueItem, history []HistoryItem) (*Issue, error)
}

type Issue struct {
    ID            string            `json:"id"`
    Type          IssueType         `json:"type"`
    Severity      Severity          `json:"severity"`
    QueueItem     QueueItem         `json:"queueItem"`
    RelatedHistory []HistoryItem    `json:"relatedHistory,omitempty"`
    Details       map[string]any    `json:"details"`
    DetectedAt    time.Time         `json:"detectedAt"`
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
    rules []SafetyRule
}

func (e *Engine) Evaluate(ctx context.Context, issue Issue) (*Decision, error)

type Decision struct {
    Issue      Issue                `json:"issue"`
    Rule       SafetyRule           `json:"rule"`
    Action     Action               `json:"action"`
    Conditions []ConditionResult    `json:"conditions"`
    Approved   bool                 `json:"approved"`
    Reason     string               `json:"reason,omitempty"` // if denied
    Timestamp  time.Time            `json:"timestamp"`
    DryRun     bool                 `json:"dryRun"`
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
3. Check global constraints (always enforced).
4. If all pass → `Approved = true`.
5. If `DryRun == true` → `Approved = true` but `Decision.DryRun = true`.
6. Log the full decision.

### 5.5 Action Executor (`internal/executor/executor.go`)

```go
type Executor struct {
    sonarrClient   *sonarr.Client
    notifier       *notifications.Notifier
    dryRun         bool
}

func (e *Executor) Execute(ctx context.Context, decision Decision) error
```

Dispatches to specific action handlers based on `decision.Action.Type`.

Each handler is a self-contained function that:
1. Checks `dryRun` — if true, logs and returns nil.
2. Performs the Sonarr API call(s).
3. Logs the outcome.
4. Fires notification if configured.

---

## 6. Data Models

### Core Domain Types

```go
// QueueItem represents a Sonarr queue item (simplified from Sonarr API response).
type QueueItem struct {
    ID                int       `json:"id"`
    SeriesID          int       `json:"seriesId"`
    EpisodeID         int       `json:"episodeId"`
    SeriesTitle       string    `json:"seriesTitle"`
    EpisodeTitle      string    `json:"episodeTitle"`
    Quality           string    `json:"quality"`
    Size              int64     `json:"size"`
    Status            string    `json:"status"` // queued, paused, downloading, completed, warning, failed
    TrackedDownloadStatus string `json:"trackedDownloadStatus"` // ok, warning, error
    TrackedDownloadState   string `json:"trackedDownloadState"` // downloading, importPending, importing, imported, importFailed, downloadFailed
    StatusMessages    []StatusMessage `json:"statusMessages"`
    ErrorMessage      string    `json:"errorMessage"`
    DownloadID        string    `json:"downloadId"`
    DownloadClient    string    `json:"downloadClient"`
    Added             time.Time `json:"added"`
}

type StatusMessage struct {
    Title    string   `json:"title"`
    Messages []string `json:"messages"`
}

// HistoryItem represents a Sonarr history record.
type HistoryItem struct {
    ID         int       `json:"id"`
    SeriesID   int       `json:"seriesId"`
    EpisodeID  int       `json:"episodeId"`
    SourceTitle string   `json:"sourceTitle"`
    EventType  string    `json:"eventType"` // grabbed, downloadFolderImported, downloadFailedImport, downloadIgnored, episodeFileDeleted
    Quality    string    `json:"quality"`
    Date       time.Time `json:"date"`
    Data       map[string]string `json:"data"` // additional info per event type
}

// ManualImportRequest is sent to Sonarr's manual import endpoint.
type ManualImportRequest struct {
    Path         string       `json:"path"`
    SeriesID     int          `json:"seriesId"`
    SeasonNumber int          `json:"seasonNumber"`
    EpisodeIDs   []int        `json:"episodeIds"`
    Quality      QualityModel `json:"quality"`
    Language     LanguageModel `json:"language"`
    DownloadID   string       `json:"downloadId"`
    ImportMode   string       `json:"importMode"` // move or copy
}

type QualityModel struct {
    Quality  Quality  `json:"quality"`
    Revision Revision `json:"revision"`
}

type Quality struct {
    ID int `json:"id"`
}

type Revision struct {
    Version int `json:"version"`
}

type LanguageModel struct {
    ID int `json:"id"`
}

// ParseResult is returned by Sonarr's parse endpoint.
type ParseResult struct {
    Title        string         `json:"title"`
    ParsedEpisodeInfo *ParsedEpisodeInfo `json:"parsedEpisodeInfo"`
    Series       *SeriesInfo    `json:"series"`
    Episodes     []EpisodeInfo  `json:"episodes"`
}

type ParsedEpisodeInfo struct {
    ReleaseTitle     string   `json:"releaseTitle"`
    SeriesTitle      string   `json:"seriesTitle"`
    SeasonNumber     int      `json:"seasonNumber"`
    EpisodeNumbers   []int    `json:"episodeNumbers"`
    AbsoluteEpisodeNumbers []int `json:"absoluteEpisodeNumbers"`
    FullSeason       bool     `json:"fullSeason"`
    Quality          QualityModel `json:"quality"`
    Language         LanguageModel `json:"language"`
}

type SeriesInfo struct {
    Title  string `json:"title"`
    TVDBID int    `json:"tvdbId"`
    TVDBID int    `json:"tvdbId"`
    ImdbID string `json:"imdbId"`
}

type EpisodeInfo struct {
    ID            int    `json:"id"`
    EpisodeNumber int    `json:"episodeNumber"`
    SeasonNumber  int    `json:"seasonNumber"`
    Title         string `json:"title"`
}
```

---

## 7. Rule Engine

### Built-in Rules (Derived from Config)

The configuration file is compiled into safety rules at startup. Each configurable automation setting generates one or more rules internally.

**Example: How config becomes rules:**

```yaml
automation:
  removeNotCustomFormat:
    enabled: true
    waitHours: 2
    deleteFiles: false
```

Generates internal rule:

```go
SafetyRule{
    ID: "remove_not_custom_format",
    Description: "Remove completed downloads that Sonarr determined are not custom format upgrades",
    Enabled: true,
    Conditions: []Condition{
        {Field: "queue.status", Operator: "eq", Value: "completed"},
        {Field: "queue.trackedDownloadState", Operator: "eq", Value: "importFailed"},
        {Field: "status_message", Operator: "matches", Value: "(?i)not.*(custom format|an upgrade)"},
        {Field: "age_hours", Operator: "gte", Value: "2"},
        {Field: "currently_importing", Operator: "eq", Value: "false"},
    },
    Action: Action{
        Type: "remove_from_queue",
        Params: map[string]string{
            "removeFromClient": "true",
            "deleteFiles": "false",
        },
    },
}
```

### Global Safety Constraints (Non-Configurable)

These are always enforced, regardless of configuration:

1. **No duplicate actions**: If the same item triggered the same rule and the action was executed within the last 5 minutes, skip.
2. **Cooldown period**: After any destructive action on a series/episode pair, wait at least 30 minutes before acting again (prevents thrashing).
3. **Sonarr connectivity required**: Do not attempt any action if the last health check failed.
4. **Atomicity**: For actions that touch multiple systems (queue + client + files), if the first step fails, do not proceed to subsequent steps.

---

## 8. Configuration Schema

### Complete YAML Schema

```yaml
# Sonarr Recovery Agent Configuration
# Version: 1.0.0

# ─── Sonarr Connection ───
sonarr:
  url: http://sonarr:8989          # REQUIRED. Sonarr base URL.
  apiKey: ""                        # REQUIRED. Sonarr API key.
  timeout: 30s                      # HTTP request timeout.
  maxConcurrency: 5                 # Max concurrent Sonarr API requests.
  retryAttempts: 3                  # Retry attempts for polling calls.
  retryBackoff: exponential         # exponential, linear, or fixed.

# ─── Download Client ───
downloadClient:
  enabled: true                     # Enable download client integration (for removal).

# ─── Monitoring ───
monitoring:
  queueInterval: 30s                # Queue polling interval.
  historyInterval: 5m               # History polling interval.
  healthInterval: 60s               # Sonarr health check interval.
  startupDelay: 10s                 # Delay before first poll cycle.

# ─── Automation ───
automation:

  # ── Remove "Not a Custom Format Upgrade" downloads ──
  removeNotCustomFormat:
    enabled: true
    waitHours: 2                    # Minimum age before removal.
    deleteFiles: false              # Also delete files from disk.
    blacklistRelease: false         # Also blacklist the release.
    statusMessageRegex: ""          # Custom regex to match (defaults to built-in).

  # ── Remove broken/stuck downloads ──
  removeBrokenDownloads:
    enabled: true
    waitHours: 6                    # Minimum age before removal.
    deleteFiles: false
    blacklistRelease: false
    errorConditions:                # Which error states trigger removal.
      - missing_files               # "No files found are eligible for import"
      - download_client_error       # Download client reports error
      - abandoned                   # Completed but no import attempt within waitHours

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
    enabled: false                  # DEFAULT OFF: Enable only after dry-run testing.
    minimumConfidence: 95           # Auto-import if confidence >= this.
    manualReviewThreshold: 70       # Create review suggestion if confidence >= this (and < minimumConfidence).

  # ── Intelligent cleanup ──
  cleanup:
    enabled: false                  # DEFAULT OFF.
    interval: 1h                    # How often to run cleanup.
    actions:
      removeEmptyFolders:
        enabled: false
        paths: []                   # Empty = use download client paths from Sonarr.
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
  authToken: ""                     # If set, required for dashboard access.
  corsOrigins: []                   # If behind a reverse proxy.

# ─── Notifications ───
notifications:
  discordWebhook: ""                # Discord webhook URL.
  slackWebhook: ""                  # Slack webhook URL.
  gotify:
    url: ""
    token: ""
    priority: 5
  ntfy:
    url: ""                         # Default: https://ntfy.sh
    topic: ""
    token: ""                       # Optional auth token.
    priority: 3
  webhook:
    url: ""
    method: POST
    headers: {}
    bodyTemplate: ""                # Go template for custom body.
  email:
    enabled: false
    smtpHost: ""
    smtpPort: 587
    smtpUsername: ""
    smtpPassword: ""
    from: ""
    to: []

  events:                           # Per-event channel overrides.
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
dryRun: true                        # DEFAULT TRUE for safety. Set to false to enable actions.
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
| `GET` | `/api/v1/activity` | Recent decisions/actions feed (limit/offset query params) |
| `GET` | `/api/v1/config` | Read-only configuration summary (API key masked) |
| `GET` | `/health` | Health check (returns 200) |
| `GET` | `/health/sonarr` | Sonarr connectivity check |
| `GET` | `/metrics` | Prometheus metrics endpoint |

### Authentication

Dashboard endpoints are protected by a simple token check when `dashboard.authToken` is configured:

```
Authorization: Bearer <authToken>
```

Unauthenticated requests receive `401 Unauthorized`. The `/health*` and `/metrics` endpoints are not protected.

---

## 10. Dashboard

### Technology

- **Single HTML file** with embedded CSS and JavaScript.
- **Zero external dependencies** (no npm, no frameworks, no CDN).
- **Served via Go's `embed.FS`** — no build step.

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
│  │        Confidence: 98%  Method: Auto       ││
│  ├─────────────────────────────────────────────┤│
│  │ 09:15  Suggestion Created                  ││
│  │        Better.Call.Saul.S06E03.mkv         ││
│  │        Confidence: 85%  [Approve] [Reject] ││
│  └─────────────────────────────────────────────┘│
│                                                 │
└─────────────────────────────────────────────────┘
```

---

## 11. Notifications

### Discord Notification Template

```json
{
  "embeds": [{
    "title": "Recovered Import",
    "description": "**Series:** Foundation\n**Episode:** S02E08\n**Method:** Automatic Manual Import\n**Confidence:** 98%",
    "color": 3066993,
    "timestamp": "2026-07-23T09:42:00Z",
    "footer": {"text": "Sonarr Recovery Agent"}
  }]
}
```

### Slack Notification Template

```json
{
  "blocks": [
    {"type": "header", "text": {"type": "plain_text", "text": "Recovered Import"}},
    {"type": "section", "fields": [
      {"type": "mrkdwn", "text": "*Series:*\nFoundation"},
      {"type": "mrkdwn", "text": "*Episode:*\nS02E08"},
      {"type": "mrkdwn", "text": "*Method:*\nAutomatic Manual Import"},
      {"type": "mrkdwn", "text": "*Confidence:*\n98%"}
    ]}
  ]
}
```

---

## 12. Metrics & Observability

### Prometheus Metrics (Detailed)

All metrics are registered under the `sonarr_recovery_` namespace.

```
# HELP sonarr_recovery_imports_recovered_total Number of successful automatic manual imports.
# TYPE sonarr_recovery_imports_recovered_total counter
sonarr_recovery_imports_recovered_total{confidence_bucket="95-100"} 5

# HELP sonarr_recovery_downloads_removed_total Number of downloads removed by rule.
# TYPE sonarr_recovery_downloads_removed_total counter
sonarr_recovery_downloads_removed_total{reason="not_custom_format"} 14
sonarr_recovery_downloads_removed_total{reason="stuck_download"} 3

# HELP sonarr_recovery_retries_total Number of import retries attempted.
# TYPE sonarr_recovery_retries_total counter
sonarr_recovery_retries_total{outcome="success"} 2
sonarr_recovery_retries_total{outcome="failed"} 1

# HELP sonarr_recovery_decisions_evaluated_total Number of safety rule evaluations.
# TYPE sonarr_recovery_decisions_evaluated_total counter
sonarr_recovery_decisions_evaluated_total{rule="remove_not_custom_format",passed="true"} 42
sonarr_recovery_decisions_evaluated_total{rule="remove_not_custom_format",passed="false"} 12

# HELP sonarr_recovery_queue_items_observed Current number of queue items observed.
# TYPE sonarr_recovery_queue_items_observed gauge
sonarr_recovery_queue_items_observed 18

# HELP sonarr_recovery_suggestions_pending Current number of import suggestions awaiting review.
# TYPE sonarr_recovery_suggestions_pending gauge
sonarr_recovery_suggestions_pending 2

# HELP sonarr_recovery_sonarr_up Whether Sonarr is reachable (1) or not (0).
# TYPE sonarr_recovery_sonarr_up gauge
sonarr_recovery_sonarr_up 1

# HELP sonarr_recovery_cycle_duration_seconds Duration of each monitoring cycle.
# TYPE sonarr_recovery_cycle_duration_seconds histogram
sonarr_recovery_cycle_duration_seconds_bucket{monitor="queue",le="0.5"} 100
```

### Health Check

```json
// GET /health
{
  "status": "healthy",
  "version": "1.0.0",
  "uptime_seconds": 264000
}

// GET /health/sonarr
{
  "status": "healthy",
  "sonarr_version": "4.0.0.741",
  "auth_valid": true
}
```

---

## 13. Testing Strategy

### Unit Tests

| Layer | Coverage Target | Focus |
|---|---|---|
| `internal/safety/` | 95%+ | Rule evaluation, condition logic, edge cases |
| `internal/recovery/` | 90%+ | Confidence scoring, file matching, parse result interpretation |
| `internal/config/` | 90%+ | Config loading, env var overrides, defaults, validation |
| `internal/detectors/` | 85%+ | Issue detection logic with mocked Sonarr data |
| `internal/executor/` | 85%+ | Action dispatch, dry-run behavior, error handling |
| `internal/notifications/` | 80%+ | Template rendering, webhook payload construction |

### Integration Tests

- **Sonarr API Mock:** An `httptest.Server` that simulates Sonarr API responses (queue, history, parse, manual import).
- **End-to-end scenarios:**
  1. Stuck download detected → safety check passes → removal action occurs (or dry-run skips).
  2. "Not a Custom Format Upgrade" detected → wait time satisfied → removal occurs.
  3. Failed import → recovery scans directory → parse succeeds → confidence high → auto-import.
  4. Failed import → recovery scans directory → confidence medium → suggestion created.
  5. Failed import → transient error → retry schedule → recovery on retry N.
  6. Dry-run enabled → all detections work, zero mutations occur.

### Test Fixtures

Use real anonymized Sonarr API responses stored as JSON fixtures for consistent, repeatable tests.

---

## 14. Deployment

### Docker

**Dockerfile:**

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /sonarr-remediator ./cmd/sonarr-remediator

FROM alpine:3.20
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /sonarr-remediator /usr/local/bin/sonarr-remediator
EXPOSE 8080
USER 1000:1000
ENTRYPOINT ["sonarr-remediator"]
CMD ["--config", "/config/config.yaml"]
```

### Docker Compose (with Sonarr)

```yaml
version: "3.8"
services:
  sonarr:
    image: linuxserver/sonarr:latest
    volumes:
      - ./sonarr/config:/config
      - ./data:/data
    environment:
      - PUID=1000
      - PGID=1000

  sonarr-remediator:
    image: ghcr.io/user/sonarr-remediator:latest
    volumes:
      - ./data:/data:ro              # Read-only access for file scanning
      - ./remediator-config:/config  # Configuration directory
    environment:
      - SRA_SONARR__URL=http://sonarr:8989
      - SRA_SONARR__API_KEY=${SONARR_API_KEY}
      - SRA_DRY_RUN=true
    ports:
      - "8080:8080"
    depends_on:
      - sonarr
```

### Configuration Volume

Mount a directory containing `config.yaml` at `/config`. The agent watches the config file for changes and hot-reloads (SIGHUP or inotify, depending on implementation choice).

---

## 15. Phase Roadmap

### Phase 1 — Core Foundation (MVP)

**Goal:** Prove the concept with the most impactful features.

| Feature | Status |
|---|---|
| Configuration loading (YAML + env) | Must have |
| Sonarr API client (queue, history, status) | Must have |
| Queue monitor (polling + state tracking) | Must have |
| History monitor (polling) | Must have |
| "Not a Custom Format Upgrade" detector | Must have |
| Stuck download detector | Must have |
| Safety engine with built-in rules | Must have |
| Dry-run mode | Must have |
| Structured JSON logging | Must have |
| Action executor (queue removal only) | Must have |
| Docker image | Must have |
| Docker Compose example | Must have |

**Phase 1 Deliverable:** A container that monitors Sonarr, detects stuck downloads and "not an upgrade" items, logs what it would do (dry-run), and will remove them (when dry-run disabled).

---

### Phase 2 — Recovery & Visibility

| Feature | Status |
|---|---|
| Import recovery engine (file scan + parse + match) | Must have |
| Confidence scoring algorithm | Must have |
| Manual import suggestion system | Must have |
| Manual import execution (auto + manual approval) | Must have |
| Retry engine (configurable intervals + retryable error patterns) | Must have |
| Dashboard (status, stats, queue, activity feed, suggestion review) | Must have |
| Dashboard auth token | Should have |
| Discord notification | Must have |
| Slack notification | Should have |
| Gotify notification | Should have |
| ntfy notification | Should have |
| Generic webhook notification | Should have |

**Phase 2 Deliverable:** Full recovery agent with dashboard, notifications, and manual review workflow.

---

### Phase 3 — Polish & Production Readiness

| Feature | Status |
|---|---|
| Intelligent cleanup engine | Should have |
| Prometheus metrics endpoint | Must have |
| Health endpoints (`/health`, `/health/sonarr`) | Must have |
| Email notification (SMTP) | Should have |
| Config hot-reload | Could have |
| Custom rule definitions in config (user-defined rules beyond built-ins) | Could have |
| Off-peak scheduling (only run cleanup during defined hours) | Could have |
| Performance tuning (concurrency limits, rate limiting) | Must have |
| Comprehensive integration tests | Must have |
| Documentation site / user guide | Must have |

**Phase 3 Deliverable:** Production-ready release with metrics, docs, and full test coverage.

---

### Phase 4 — Multi-*arr Support

| Feature | Status |
|---|---|
| Refactor to `pkg/arrclient` — generic *arr API client | — |
| Sonarr adapter (production) | Migration |
| Radarr adapter (new) | — |
| Lidarr adapter (new) | — |
| Readarr adapter (new) | — |
| Multi-instance support (one agent per *arr, or single agent multi-app) | — |
| Unified dashboard (all *arr instances in one view) | — |
| Shared rule engine (rules that apply across *arr types) | — |

**Phase 4 Deliverable:** Generic Arr Recovery Agent supporting the full *arr ecosystem.

---

## 16. Long-Term Vision

Rather than being a "Sonarr cleanup script," this project should become a generic **Arr Recovery Agent** capable of autonomously monitoring, recovering, and maintaining the health of media automation stacks.

### Core Design Principles (Eternal)

1. **Safety first, automation second.** The system should prefer observation and reporting over risky assumptions. Users should build confidence through dry-run mode before enabling automated recovery.

2. **Transparency.** Every action must include a human-readable explanation. The decision log must show exactly which rules fired, which conditions passed or failed, and why an action was taken or skipped.

3. **Explainability.** A non-technical user running the dashboard should be able to understand why the agent did something. Log messages should be in plain English, not stack traces or raw API responses.

4. **Configurability.** Nothing should be hardcoded. Every threshold, every matching pattern, every action parameter should be configurable. Defaults should be safe and conservative.

5. **Modularity.** The core architecture should be modular, with reusable components for queue monitoring, rule evaluation, recovery workflows, cleanup actions, notifications, metrics, and dashboards. Support for Sonarr is the initial implementation, but the design should make it straightforward to extend to Radarr, Lidarr, Readarr, or other automation tools in the future by implementing application-specific API adapters.

6. **Reversibility.** Where possible, actions should be reversible. A removed download can be re-added (manually). An imported file can be deleted. The system should never perform irreversible destructive actions without explicit user configuration.

---

## 17. Appendix: Sonarr API Surface

The following Sonarr v3/v4 API endpoints are used by this agent:

### Queue
- `GET /api/v3/queue` — List queue items (active + queued downloads).
- `GET /api/v3/queue/details` — Queue items with episode details.
- `DELETE /api/v3/queue/{id}` — Remove item from queue.
  - Query params: `removeFromClient=true`, `blocklist=true`, `blocklist=false`

### History
- `GET /api/v3/history` — History of all events.
  - Query params: `page`, `pageSize`, `sortKey`, `sortDirection`, `eventType`, `episodeId`, `seriesId`

### Manual Import
- `GET /api/v3/manualimport` — Get files available for manual import.
  - Query params: `folder`, `downloadId`, `seriesId`, `filterExistingFiles`
- `POST /api/v3/manualimport` — Execute manual import.
  - Body: `ManualImportRequest`

### Parse
- `GET /api/v3/parse` — Parse a file path or title.
  - Query params: `path`, `title`

### System
- `GET /api/v3/system/status` — System status (version, health).

### Blacklist
- `POST /api/v3/blacklist` — Add item to blacklist.
  - Body: `{ "seriesId": int, "episodeIds": [int], "sourceTitle": string, "quality": QualityModel, "date": string }`
- `DELETE /api/v3/blacklist/{id}` — Remove from blacklist.

### Episode
- `GET /api/v3/episode/{id}` — Get episode details.
- `GET /api/v3/episode` — List episodes (with `seriesId` filter).

### Series
- `GET /api/v3/series/{id}` — Get series details.
- `GET /api/v3/series` — List all series.

### Download Client (Sonarr Redirect)
- Queue removal with `removeFromClient=true` will instruct Sonarr to forward the removal to the connected download client.

---

*End of specification.*
