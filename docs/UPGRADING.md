# Upgrading and Rollback

Use a published release tag (`v<VERSION>`) or digest for controlled upgrades.
`latest` follows stable releases and is convenient for evaluation, but does not
pin a deployment. Read the target release notes before updating, especially when
cleanup is enabled.

## Update Procedure

1. Inspect pending/uncertain actions with `actions` and manual receipts with
   `triggers`; reconcile uncertain external effects in Sonarr.
2. Stop the observer and make a private [whole-state backup](OPERATIONS.md#backup-and-restore).
3. Pull the selected image and recreate the container with the same configuration,
   identity, security options, network and persistent state mount.
4. Inspect `version`, `status --json`, logs and health before enabling new behavior.

For Compose, change `image` to the chosen release, run `docker compose pull`,
then `docker compose up -d` after the stopped-state backup. Do not delete the
state volume. For Podman, recreate the container using the deployment command
and the existing state directory.

## Compatibility Boundaries

| Version | Operator impact |
| --- | --- |
| 0.5.0 | Breaking cleanup behavior: Sonarr's `Caution: Found executable file` queue warning can delete the shared download and client data on the first eligible scan, even with an empty preview. Disable cleanup before upgrading if this deletion is unwanted. Image-version update only; no configuration or state migration. |
| 0.4.1 | INFO logs use periodic service summaries; detailed decisions require DEBUG. Image-version update only. |
| 0.4.0 | Breaking cleanup behavior: permanent episode/folder mismatches can delete the shared download and client data on the first eligible scan, without the six-hour wait. Disable cleanup if this earlier deletion is unwanted. No configuration or state migration is required. |
| 0.3.0 | Cleanup broadens to valid persistent Sonarr rejections previously kept for manual review, including mapping/access blockers. Keep `cleanup.enabled: false` if automatic deletion is unwanted. Existing settings/schema-2 history remain valid; new rejected-time fields start at zero. |
| 0.2.1 | Manual `--override-wait` also bypasses episode cooldown for that request. Background behavior is unchanged. |
| 0.2.0 | Validated schema-1 stores automatically migrate to schema 2 transactionally, preserving waits, budgets and unresolved effects. Version 0.1.x cannot read the upgraded store. |

These changes do not require new mounts, ports, container identity, healthchecks
or startup commands. Deployment changes requiring more than an image-version
update are treated as breaking changes and must include migration instructions.

## Rollback

Stop the service and back up state before selecting an older image. Confirm the
older binary supports the current state schema; unsupported schemas fail before
observation. Image downgrade alone cannot roll a schema-2 store back to 0.1.x.

Restoring an older state backup can erase knowledge of actions Sonarr already
accepted. Reconcile all later effects before any restore, and preserve uncertainty
protection. Do not use state deletion as a rollback strategy.
