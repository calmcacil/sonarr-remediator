# Operational State and Log Noise Plan

## Current Status

Phase 1 is complete. Repeated safety skips now use a stable suppression
identity based on the item, action, and failed check, so changing values such
as `got 22m0s ago` no longer create a new info-level log record. Routine
detector matches are debug-level, while action recommendations, actions, and
skips remain visible at info level.

The next phase should add durable operational state without immediately
changing how Sonarr mutations are executed.

## Deployment Direction

The observer and action worker should run inside one long-lived Go process in
one rootless Podman container. They should be separate application components,
not separate deployed services:

```text
one sonarr-remediator container
└── one Go process
    ├── observer loop
    │   └── Sonarr reads -> detection -> SQLite
    ├── worker loop
    │   └── SQLite claims -> revalidation -> Sonarr writes -> SQLite
    └── maintenance
        └── cleanup and expired-claim recovery
```

Do not add a second container, an HTTP endpoint, or a shell-based process
supervisor for this work. The workload does not justify independent scaling or
separate service lifecycles.

The current read-only container contract will need one narrowly scoped
writable state mount:

```yaml
services:
  sonarr-remediator:
    image: ghcr.io/calmcacil/sonarr-remediator:latest
    user: "${PUID:-1000}:${PGID:-1000}"
    read_only: true
    volumes:
      - ./remediator-config:/config:ro
      - ./data:/data:ro
      - ./remediator-state:/state
```

The database should live at `/state/remediator.db`. The host state directory
must be writable by the configured rootless UID/GID. The root filesystem,
`/config`, and `/data` remain read-only.

## Phase 2: SQLite Operational State and CLI

Phase 2 should first persist the current workflow while retaining synchronous
execution:

```text
poll -> detect -> safety -> execute -> record outcome
```

This establishes the schema, migrations, retention, reconciliation, volume
permissions, and CLI behavior before SQLite becomes the execution coordinator.

SQLite should complement logs rather than replace them. Logs remain appropriate
for lifecycle events, API failures, action attempts, and unexpected errors.

### Initial Schema

Keep current issue state, decisions, and action history distinct:

```sql
CREATE TABLE issues (
    issue_key        TEXT PRIMARY KEY,
    issue_type       TEXT NOT NULL,
    item_key         TEXT NOT NULL,
    queue_item_id    INTEGER,
    series_id        INTEGER,
    episode_id       INTEGER,
    download_id      TEXT,
    state            TEXT NOT NULL,
    first_seen_at    TEXT NOT NULL,
    last_seen_at     TEXT NOT NULL,
    resolved_at      TEXT,
    details_json     TEXT NOT NULL
);

CREATE TABLE action_attempts (
    id               INTEGER PRIMARY KEY,
    issue_key        TEXT NOT NULL,
    action           TEXT NOT NULL,
    outcome          TEXT NOT NULL,
    dry_run          INTEGER NOT NULL,
    reason           TEXT,
    attempted_at     TEXT NOT NULL,
    completed_at     TEXT,
    details_json     TEXT,
    FOREIGN KEY (issue_key) REFERENCES issues(issue_key)
);
```

Useful issue states are `active` and `resolved`. Detailed execution states
belong to action requests in Phase 3 rather than making the issue table a
workflow engine.

Use schema versioning through `PRAGMA user_version`. Configure SQLite for
local concurrent daemon/CLI access with WAL mode, foreign keys, and a bounded
busy timeout. The daemon should initially use one database connection because
the expected workload is small; CLI commands use a short-lived read
connection.

### Reconciliation and Retention

Only reconcile missing issues after a complete, successful queue scan. A queue
fetch failure, connectivity failure, cancellation, or partially processed poll
must not resolve everything. Mark absent issues resolved, retain resolved rows
for a bounded period such as 30 days, and periodically delete old resolved
issues and action attempts.

### CLI Access

The CLI should be a subcommand of the existing binary and should open the same
database without starting monitors:

```text
sonarr-remediator status --config /config/config.yaml
sonarr-remediator issues --config /config/config.yaml
sonarr-remediator issues --state active --config /config/config.yaml
sonarr-remediator show <issue-key> --config /config/config.yaml
sonarr-remediator actions --since 24h --config /config/config.yaml
```

In production, invoke it through the running rootless container:

```bash
podman exec sonarr-remediator \
  /sonarr-remediator status --config /config/config.yaml
```

Commands should default to a compact human-readable table and support
`--json` for scripting. Query commands can use a read-only `/state` mount when
run as a one-shot container, while cleanup or migrations require a writable
mount. `podman exec` is the normal path while the daemon is running.

The CLI should not require an HTTP port, inbound authentication, a shell in the
distroless image, or elevated Podman permissions.

## Phase 3: Durable Internal Action Worker

After Phase 2 has established reliable persistence, move Sonarr mutations
behind a durable action queue inside the same process:

```text
observer -> action_requests table -> internal worker -> Sonarr writes
```

The observer owns facts and desired actions. The worker owns claims,
revalidation, Sonarr mutations, verification, retries, and action outcomes.
Both components log their own events.

### Action Requests

Add a separate request table rather than overloading `issues`:

```sql
CREATE TABLE action_requests (
    id               INTEGER PRIMARY KEY,
    issue_key        TEXT NOT NULL,
    action           TEXT NOT NULL,
    state            TEXT NOT NULL,
    not_before       TEXT NOT NULL,
    attempt_count    INTEGER NOT NULL DEFAULT 0,
    claimed_by       TEXT,
    claim_expires_at TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    completed_at     TEXT,
    last_error       TEXT,
    FOREIGN KEY (issue_key) REFERENCES issues(issue_key)
);

CREATE UNIQUE INDEX one_active_request
ON action_requests(issue_key, action)
WHERE state IN ('pending', 'waiting', 'claimed', 'retryable');
```

Request states should be precise:

```text
pending
waiting
claimed
succeeded
retryable
failed
cancelled
recommended
```

Use `succeeded` rather than `handled`; “handled” is ambiguous between claimed,
attempted, skipped, and successfully completed.

### Worker Safety

The worker must atomically claim a due request, set a short lease, and commit
before calling Sonarr. If the process exits, another worker cycle can recover
expired claims. Even with one worker today, claim semantics should be safe for
future process separation.

Before every Sonarr mutation, the worker must fetch current state and re-run
the relevant safety checks. A queued decision can become stale because the
item may disappear, start importing, acquire a file, or be superseded by a
newer release.

Each action type needs its own completion rule. Do not mark an action
successful solely because Sonarr returned HTTP 200 or 202 when the expected
result can be verified through a later queue or history read.

Cooldowns and retries should become persisted scheduling data:

```text
state=waiting
not_before=2026-08-17T12:03:43+02:00
```

This replaces repeated polling-time rejections with one persisted waiting
request. Existing retry timers can eventually use the same mechanism, making
pending retries survive restarts.

Dry-run recommendations must never be executable work. Store them as terminal
`recommended` records or decision history, and require `dry_run = false` for
worker claims.

### Internal Loops

The single Go process can use goroutines managed by one root context:

```text
queue observer
health monitor
action worker
maintenance and retention
```

The worker should wake through an in-memory notification channel when the
observer creates work, a timer for the earliest `not_before`, and a modest
fallback poll for restart resilience. SQLite remains authoritative; the
notification channel is only an optimization.

Shutdown should stop new scans, stop new claims, allow the current scan and
in-flight action to finish within the existing timeout, and leave an unfinished
request recoverable through its lease if the timeout expires.

## Optional Future Phase: Process Separation

Separate `observe` and `work` process modes are not part of the initial Phase 3
scope. They should only be considered if independent lifecycle management,
fault isolation, or scaling becomes necessary:

```text
sonarr-remediator observe
sonarr-remediator work
```

If this ever becomes necessary, both processes must use the same local state
volume and rootless UID/GID. SQLite should not be placed on NFS, SMB, or
another network filesystem for this purpose. The one-container, one-process
deployment remains the preferred production model.

## Implementation Sequence

1. Phase 1: reduce repeated polling logs. **Complete.**
2. Phase 2: add SQLite state, migrations, retention, and read-only CLI queries.
3. Phase 3: add the internal durable action queue and worker.
4. Fold cooldown and retry scheduling into persisted requests.
5. Add verified action outcomes and expired-claim recovery.
6. Consider separate process modes only if operational needs justify them.

Avoid a generic workflow engine or event-sourcing model. Keep the database
focused on current issue state, durable action requests, and concise action
history.
