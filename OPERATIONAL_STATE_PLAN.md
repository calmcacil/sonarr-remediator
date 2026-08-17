# Operational State and Log Noise Plan

## Recommendation

SQLite can provide useful operational state, but it should not be the first fix
for the current log volume. The immediate problem is repeated polling output,
not the absence of a database.

The service already has short-lived in-memory state for duplicate actions,
cooldowns, retries, repeated-skip suppression, and a bounded decision ring. The
first phase should make those existing controls effective and make routine
detection logs debug-level. Logs should remain the interface for lifecycle
events, API failures, action attempts, and unexpected errors.

## Phase 1: Quieter Event Logging

1. Suppress repeated skips using a stable identity composed of the item,
   action, and failed check. Do not include the rendered reason because values
   such as `got 22m0s ago` change on every poll.
2. Keep the complete reason and checks in the emitted record for diagnostics.
3. Log routine detector matches at debug level. The meaningful info-level
   transition is the resulting action recommendation, action, or skip.
4. Prefer stable cooldown data such as a future eligibility timestamp in a
   later refinement; elapsed values can remain useful at debug level.
5. Add regression tests proving that changing cooldown elapsed text does not
   defeat suppression.

This should reduce an unchanged item from one info line per poll to one initial
message and, at most, a periodic reminder governed by the suppression window.

## Phase 2: SQLite Operational State

SQLite is worthwhile when the service needs restart-safe state, a queryable
list of unresolved issues, action history, or a CLI status view. It should
complement logs rather than replace them.

Keep the initial schema small:

```sql
CREATE TABLE issues (
    issue_key       TEXT PRIMARY KEY,
    issue_type      TEXT NOT NULL,
    item_key        TEXT NOT NULL,
    queue_item_id   INTEGER,
    series_id       INTEGER,
    episode_id      INTEGER,
    download_id     TEXT,
    action          TEXT NOT NULL,
    state           TEXT NOT NULL,
    blocking_check  TEXT,
    blocking_reason TEXT,
    first_seen_at   TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL,
    next_action_at  TEXT,
    resolved_at     TEXT
);

CREATE TABLE actions (
    id              INTEGER PRIMARY KEY,
    issue_key       TEXT NOT NULL,
    action          TEXT NOT NULL,
    outcome         TEXT NOT NULL,
    dry_run         INTEGER NOT NULL,
    reason          TEXT,
    attempted_at    TEXT NOT NULL,
    FOREIGN KEY (issue_key) REFERENCES issues(issue_key)
);
```

Useful issue states are `observed`, `blocked`, `eligible`, `scheduled`,
`handled`, `resolved`, and `failed`. Dry-run recommendations must not be
recorded as handled actions.

### Reconciliation and Retention

Only reconcile missing issues after a complete, successful queue scan. A queue
fetch failure, connectivity failure, cancellation, or partially processed poll
must not resolve everything. Mark absent issues resolved, retain resolved rows
for a bounded period such as 30 days, and periodically delete old resolved
issues and action records.

### CLI

Prefer read-only subcommands on the existing executable:

```text
sonarr-remediator status
sonarr-remediator issues
sonarr-remediator issues --state blocked
sonarr-remediator show <issue-key>
sonarr-remediator actions --since 24h
sonarr-remediator cleanup
```

The default output should be a compact table, with `--json` for scripting.
SQLite should use WAL mode and a busy timeout because the daemon and CLI may
access the database concurrently. In Docker, put the database under a
configurable persistent volume such as `/data/sonarr-remediator.db`.

### Implementation Sequence

1. Complete Phase 1 and observe the resulting logs.
2. Add a narrow domain-specific storage interface.
3. Add SQLite with schema versioning via `PRAGMA user_version`.
4. Upsert issue observations during successful scans.
5. Record safety decisions and action outcomes.
6. Reconcile absent issues only after successful full scans.
7. Add read-only status, issues, show, and actions commands.
8. Add retention cleanup and Docker volume documentation.

Avoid a generic workflow engine or event-sourcing model. The database should
track current issue state and action history with as few moving parts as
possible.
