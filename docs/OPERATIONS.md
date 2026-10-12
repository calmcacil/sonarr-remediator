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
podman build --format oci -f Containerfile -t localhost/remediator:acceptance .
python3 scripts/podman-smoke.py --image localhost/remediator:acceptance --architecture arm64
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

## Health, Logs, and Shutdown

Logs are JSON on stderr by default. Set `logging.format: text` or adjust
`logging.level` when needed. New decisions and failures are visible; identical
decisions are deduplicated. Health requires local readiness, a recent heartbeat
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

## Intervention

`show DOWNLOAD_ID` explains policy blockers and observed waiting progress.
`actions` records completed and uncertain effects. Verify uncertain work in
Sonarr using its command/history/current files. This first release provides no
automatic uncertainty override or destructive state-reset command. Fix mapping,
permissions, media inspection, or ambiguous matches in Sonarr. Complete queue
absence can resolve an untouched issue externally, but cannot prove an import
by this service.
