# Operations

For installation and settings, see [deployment](DEPLOYMENT.md) and
[configuration](CONFIGURATION.md). Commands below use Podman; use equivalent
`docker exec` commands for Docker, or `docker compose exec remediator` for the
supplied Compose service.

## Inspect Work

These commands read local SQLite state, need no API key, and send no Sonarr requests:

```sh
podman exec sonarr-remediator /sonarr-remediator status --json
podman exec sonarr-remediator /sonarr-remediator issues
podman exec sonarr-remediator /sonarr-remediator show DOWNLOAD_ID
podman exec sonarr-remediator /sonarr-remediator actions --download-id DOWNLOAD_ID --limit 20
podman exec sonarr-remediator /sonarr-remediator triggers --limit 20
```

Use Sonarr's opaque `downloadId`, not a queue-row ID, release title, or hash
extracted from a title. Query commands accept `--state PATH`; list commands accept
`--limit`. The default state path is `/state/remediator.db`, independent of the
service's configuration file. Use `version` to inspect the binary version.
Without a subcommand, the binary runs the service.

Inspection needs the state files, not a running observer. With a compatible local
binary and access as the state owner, a stopped instance can be inspected with:

```sh
sonarr-remediator status --state /path/to/state/remediator.db --json
```

## Manual Cycles

Manual control requires the running observer and its state-owner identity. Run
commands inside the existing container; the private control socket uses the
existing state mount, with no new port or mount. `status` reports socket
availability. An unsupported socket path disables manual control but leaves
ordinary observation running. A stopped or busy observer fails explicitly.

```sh
podman exec sonarr-remediator /sonarr-remediator trigger --json
podman exec sonarr-remediator /sonarr-remediator trigger --dry-run --override-wait --download-id DOWNLOAD_ID --json
podman exec sonarr-remediator /sonarr-remediator trigger --execute --download-id DOWNLOAD_ID --json
podman exec sonarr-remediator /sonarr-remediator triggers --request-id REQUEST_ID --json
```

`trigger` defaults to preview: it reads Sonarr but saves no state, recommendations,
waiting progress, receipts, or budget reservations. This differs from background
dry-run, which persists observations and recommendations. Background observation
can run before or after a preview.

`--execute` authorizes one live cycle even with background `dry_run: true`.
Configured permissions still apply; cleanup can delete downloaded data.
`--dry-run=false` is rejected. A new live evaluation can differ from an earlier
preview. Reports distinguish policy eligibility from actions that fit shared budgets.

`--override-wait` skips recovery and cleanup waits, including rejected-download
waits, and episode cooldowns for this request. It does not change stored progress
or bypass permissions, protected states, competition, retry spacing, attempt
exhaustion, mutation budgets, or uncertain effects. See [policy](POLICY.md).

Repeat `--download-id` for multiple exact IDs. Omit it to evaluate the current
queue and pending search continuations. IDs are deduplicated without alteration;
competition checks still use the full queue. Requests allow at most 1,000 IDs
and 16 KiB of request data; narrow the selection after a size error.

The default and maximum `--timeout` are `2m`, including waiting for an automatic
cycle. Only one manual request can run or queue; additional requests return
`busy`. SIGINT stops new work and returns 130, but a request Sonarr already
accepted can still finish.

### Receipts and Exit Codes

Each live request prints its ID to stderr before transmission and receives a
durable receipt before mutation. `triggers` reads receipts locally. Effects stay
pending until ordinary correlated verification, which also runs during background
dry-run. A pending replacement search needs another explicit execution or active
background work.

JSON stdout contains one schema-versioned document; diagnostics use stderr.

| Exit | Meaning |
| --- | --- |
| 0 | Preview has a selectable action, or a submission was acknowledged; not proof of external success. |
| 2 | Evaluation completed with no selectable action. |
| 1 | Invalid input, unavailable/busy control, or a required-read/local failure. |
| 3 | Execution may have happened, but its result is uncertain. |
| 130 | Interrupted; accepted external work may still finish. |

After a lost response or exit 3/130, inspect receipts and Sonarr before executing
again. The CLI never retries automatically, and restart never executes unfinished
request plans. A receipt's `exit_code` records the invocation result; linked
effects show the latest verification evidence.

## Health, Logs, and Shutdown

```sh
podman logs --tail 50 sonarr-remediator
podman exec sonarr-remediator /sonarr-remediator healthcheck
```

Logs default to JSON on stderr. At INFO, a `service summary` reports active
issues, action activity, unresolved actions, connectivity, last complete scan,
and mutation-budget use. It appears after the first cycle, then every 30 minutes
at a cycle boundary. A cycle attempting real mutations emits a summary and resets
the interval. Empty queues still receive summaries; deferred scans show degraded
connectivity and the last complete scan time. Resolved history is excluded.

Mutation attempts, successful verification, warnings, and errors remain immediate.
Routine decisions and dry-run recommendations wait for the summary. Use
`logging.level: debug` for detailed `issue decision` records; identical state/reason
pairs remain deduplicated. Manual previews do not affect summaries. Summary
counters reset on process restart; use `actions` for durable history.

Health requires local readiness, a recent heartbeat and complete scan within
three polling intervals, Sonarr connectivity, and no unresolved required-read
failure. Policy deferrals and exhausted budgets are healthy. Sonarr outages
degrade health while the observer continues retrying reads.

SIGTERM/SIGINT cancels current HTTP work and stops new scans. State closes within
25 seconds; allow 30 seconds before the engine kills the container. Sonarr may
finish an already accepted request; its durable intent is reconciled on restart.

## Backup and Restore

Stop the service, then copy the entire private state directory, including the
database, WAL/SHM files if present, and lock file. Never copy a live SQLite file
independently from its WAL. Keep backups private.

Restore with the original private permissions and instance binding, then start
exactly one observer. Reconcile actions accepted since the backup before restoring;
otherwise replay protection can be lost. Never discard pending attempts or reset
state to make uncertainty disappear. See [upgrading](UPGRADING.md) for schema
compatibility and rollback boundaries.

## Intervention

`show DOWNLOAD_ID` explains blockers and observed waits. `actions` records
completed and uncertain effects. Check uncertain work against Sonarr's command
status, history, and current files before intervening.

Fix mapping, access permissions, media inspection, or ambiguous matches in Sonarr.
The service has no automatic uncertainty override or destructive state-reset
command. Complete queue absence can resolve an untouched issue externally but
does not prove an import by this service.
