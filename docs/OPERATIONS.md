# Operations

## Configuration

Precedence is defaults, YAML, then `SRA_` environment overrides. Nested names use
double underscores, for example `SRA_SONARR__URL` and `SRA_RECOVERY__WAIT=30m`.
Booleans use `true`/`false`, durations use Go duration notation, and list overrides
use YAML sequences such as `SRA_SCOPE__INCLUDE_SERIES_IDS='[10, 11]'`.
Unknown settings and invalid values fail startup. Configure exactly one of
`sonarr.api_key` or `sonarr.api_key_file`.

`config.example.yaml` lists the complete supported schema. To enable recovery,
set `dry_run: false`. Review `status` and startup logs for effective permissions.
Enabling blocklisting or replacement search also requires `cleanup.enabled`.
Cleanup has no queue-only mode; it requests removal from the download client.
Completed `importPending` downloads are observed like `importBlocked` and
`importFailed` downloads. Automatic recovery respects `recovery.wait` (15m by
default), and cleanup respects `cleanup.wait` (6h by default). Waiting progress
starts with this observer's successful scans, not Sonarr's queue-added timestamp;
Old pending downloads retain this wait after an image update unless Sonarr
reports the permanent episode mismatch described below.
A valid Sonarr rejection starts a separate observed rejected-time clock. After
`cleanup.wait`, that rejection authorizes removal of the shared download and
downloaded data, including sample-detection failures, mapping and access blockers.
Rejection clearing resets this clock; old eligible age does not prefill it.
`show` and trigger reports expose the rejected progress. No independent upgrade
or complete pack-membership proof is needed for generic rejected cleanup, and
blocklisting/replacement search stay off for that path. Empty, malformed or
unavailable previews, active `importing` work and unresolved actions remain protected.

Permanent Sonarr `Episode ... was unexpected considering the ... folder name`
and plural `Episodes ... were unexpected ...` rejections identify episode
mismatches. Cleanup removes these failed shared downloads on the first eligible
scan without `cleanup.wait`; manual preview also reports zero wait without
`--override-wait`. Client-data deletion can include a whole pack. Other wording
and temporary rejections retain their configured wait. All scope, competition,
cooldown, budget and pending/uncertain-action gates remain, and this path never
blocklists or searches. Library files are preserved. This is a breaking behavior
change for cleanup-enabled installations in v0.4.0; updating the image
requires no configuration or state migration. Disable cleanup if this earlier
automatic deletion is unwanted.

Version 0.3.0 broadens cleanup-enabled behavior: Sonarr-rejected jobs previously
preserved for manual review now qualify for deletion after the rejected wait.
Updating the image is sufficient; existing settings and schema-2 history remain
valid. New rejected-time fields start at zero when absent from older state.
Keep `cleanup.enabled: false` if automatic client-data deletion is unwanted.
Scope uses positive series IDs and exact queue download-client names. Exclusions
take precedence over allowlists. Empty allowlists permit all otherwise eligible
work. Unknown series are checked after Sonarr matches them.

## Rootless Podman

Run as a normal account with working subordinate UID/GID mappings. Use the
`--userns=keep-id:uid=65532,gid=65532` option: the invoking host account maps to
the application's container identity. Create `config` and `state` directories
with mode 0700 and secret/config files with mode 0600. This permits private bind
mounts without changing host ownership. Mount configuration read-only and state
read-write. SELinux hosts may need the appropriate private `:Z` bind label.

Use the README recipe, adding `--network YOUR_PRIVATE_NETWORK` as needed. Do not
publish ports. Keep the root filesystem read-only, all capabilities dropped,
and `no-new-privileges` set. No media mount, container engine socket, host
network, or runtime ownership repair is required. The state path defaults to
`/state/remediator.db`; its directory and database/lock/WAL files must be private.
The service rejects a second observer using the same store.

Run one observer per Sonarr instance. Separate volumes cannot coordinate two
services. The store is bound to the canonical configured URL; a changed endpoint
requires a deliberately chosen new store after stopping the old observer.

Build/test native images with:

```sh
mise exec -- podman build --format oci -f Containerfile -t localhost/remediator:acceptance .
mise exec -- python scripts/podman-smoke.py --image localhost/remediator:acceptance --architecture arm64
```

Use `amd64` on a native amd64 machine. The harness creates and removes its own
private network, fake Sonarr, and containers. It makes no live Sonarr calls.

## Secondary Rootless Docker

Use a rootless Docker daemon and `docker build -f Containerfile`. The supplied
`compose.yaml` uses a named state volume initialized from the image's private,
UID/GID 65532 directory, a read-only root, no capabilities, a local health
command, and a 30-second stop allowance.

Docker does not implement Podman's keep-id mapping. The rootless daemon maps
container UID 65532 to a subordinate host UID. Read-only bind-mounted config
must be readable by that mapped identity. Use an explicit host ACL for that
identity (including directory traversal), or provision a private configuration
volume containing mode-0600 files owned by container UID/GID 65532 before
starting Compose. Do not expose an API key through world-readable host files.
The application does not change ownership at runtime. Confirm `docker info`
reports rootless mode, then `docker compose up --build -d` and inspect local
health/status. This recipe is secondary compatibility; native Podman is the
required acceptance path.

## Manual Triggers

Use `podman exec` or `docker exec` inside the existing container as the application
identity. The running observer owns all evaluation and submission; a stopped or
busy observer fails explicitly. `status` shows whether its private local control
socket is available. A filesystem/path that cannot support the socket disables
manual control while ordinary observation continues.

```sh
podman exec sonarr-remediator /sonarr-remediator trigger --json
podman exec sonarr-remediator /sonarr-remediator trigger --dry-run --override-wait --download-id DOWNLOAD_ID --json
podman exec sonarr-remediator /sonarr-remediator trigger --execute --override-wait --download-id DOWNLOAD_ID --json
podman exec sonarr-remediator /sonarr-remediator triggers --limit 20 --json
podman exec sonarr-remediator /sonarr-remediator triggers --request-id REQUEST_ID --json
```

`trigger` defaults to preview. Preview makes read-only Sonarr calls, but does not
save state, recommendations, waiting progress or budget reservations. Background
observation can run before or after it. The report separates policy eligibility
from the actions that fit this cycle's shared budgets.

`--execute` authorizes one live cycle even with `dry_run: true`; scheduled work
keeps its configured mode. `--dry-run=false` is rejected. An explicit
`--override-wait` skips `recovery.wait`, `cleanup.wait` and, since version 0.2.1,
`limits.episode_cooldown` for this request. This allows separate redundant downloads
of the same episode to be removed in successive manual requests, and can skip
the rejected-download cleanup wait. It does not alter
stored progress or bypass protected tracked states, permissions,
competition, retry spacing, attempt exhaustion, mutation budgets or
uncertain effects. Background work and manual requests without the flag still
respect episode cooldowns. A fresh live evaluation can differ from an earlier
preview. Imports and strict cleanup still require complete episode membership;
generic Sonarr-rejected cleanup follows the rejected-download contract instead.

Repeated `--download-id` flags select exact Sonarr download IDs, deduplicated
without changing the IDs. Queue-row IDs and titles are not selectors. Omitting
the selector includes the current queue and pending search continuations. At most
1,000 downloads and 16 KiB of request data are allowed; narrow selectors on a size
error. Selection still uses the complete queue for competing-download checks.

The default and maximum `--timeout` are `2m`, including waiting for an automatic
cycle. Only one manual request can be running or queued; additional requests get
`busy`. SIGINT stops new manual work and returns 130. An accepted Sonarr request
can still finish. Imports, client removal/blocklisting and replacement search
share the usual one-per-scan and five-per-hour default budgets. A removal can
delete downloaded data; blocklisting and replacement search are separate choices.

Each live request prints its request ID on stderr before transmission and receives
a durable receipt before mutation. `triggers` reads receipts locally without
Sonarr credentials or requests. Effects remain pending until ordinary correlated
verification, which also runs during background dry-run. A pending replacement
search needs another explicit trigger or enabled active background work.

JSON stdout contains one schema-versioned document; diagnostics use stderr.
`trigger` exit codes are 0 for a completed preview with a selectable action or an
acknowledged submission, 2 for a completed evaluation with no selectable action,
1 for invalid input, unavailable/busy control or a required-read/local failure,
and 3 when execution may have happened but its result is uncertain. Exit 0 does
not claim external success. After a lost response or exit 3/130, inspect `triggers`
and Sonarr before issuing a new execution. The CLI never retries automatically,
and restart does not execute unfinished request plans.
The receipt's `exit_code` records the invocation result; its linked effects reflect
the latest verification evidence.

## Health, Logs, and Shutdown

Logs are JSON on stderr by default. Set `logging.format: text` or adjust
`logging.level` when needed. At INFO, a compact `service summary` reports active
issue counts, action activity since the previous summary, unresolved actions,
connectivity, last complete scan, and mutation budget usage. It appears after the
first observation cycle, then every 30 minutes at a cycle boundary. A cycle that
attempts real mutations also emits one summary and resets that interval. Empty
queues still receive summaries; a deferred scan reports degraded connectivity
and the last complete scan time. Resolved history is excluded from active counts.

Mutation attempts and successful verification remain immediate INFO records;
warnings and errors remain immediate. Routine issue decisions and dry-run
recommendations wait for the next summary. Set `logging.level: debug` or
`SRA_LOGGING__LEVEL=debug` for detailed `issue decision` records, including changing
wait progress; identical state/reason pairs remain deduplicated. Manual previews
do not affect summaries. Summary counters and intervals restart with the process;
use `actions` for durable history. Version 0.4.1 needs only an image-version update.

Health requires local readiness, a recent heartbeat
and complete scan within three polling intervals, Sonarr connectivity, and no
unresolved required-read failure. Policy deferrals and budget limits are healthy.
Sonarr outages degrade health while the observer continues to retry reads.

SIGTERM/SIGINT cancels current HTTP work and stops new scans. The application
closes state within 25 seconds; configure the engine to allow 30 seconds. A
request already accepted by Sonarr can still complete. Its durable intent is
reconciled on the next start.

## Backup, Restore, and Rollback

Stop the service before backing up. Copy the entire private state directory,
including database, WAL/SHM (if present), and lock file. Do not copy a live SQLite
file independently from its WAL. Keep backups private. Restore that directory
with its original private permissions and instance binding, then start exactly
one observer. Never discard pending attempts or reset state to make uncertainty
disappear: the original request may have been applied.

Use immutable version tags/digests for rollback. Stop the running service first,
back up its state, and confirm the previous binary supports the store's schema.
An unsupported schema fails before observation. Restore a backup only after
reconciling actions performed since that backup; otherwise it can lose replay
protection. Finished history defaults to 30 days; unresolved actions and records
still needed for retries, budgets, cooldowns, or operation continuation remain.

Version 0.2.0 automatically upgrades validated schema-1 stores to schema 2 in a
transaction, preserving waits, budgets and unresolved effects. Forward updates
need only an image-version change; container identity, startup, healthcheck,
mounts and permissions stay the same. Make a private stopped-state backup under
the existing procedure before updating. Version 0.1.x cannot read schema 2, so
image downgrade alone cannot roll back the store. Never restore pre-trigger state
over later accepted Sonarr effects: reconcile those effects before any restore.

## Intervention

`show DOWNLOAD_ID` explains policy blockers and observed waiting progress.
`actions` records completed and uncertain effects. Verify uncertain work in
Sonarr using its command/history/current files. The service provides no
automatic uncertainty override or destructive state-reset command. Fix mapping,
permissions, media inspection, or ambiguous matches in Sonarr. Complete queue
absence can resolve an untouched issue externally, but cannot prove an import
by this service.
