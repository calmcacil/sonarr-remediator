# Configuration

Use [config.example.yaml](../config.example.yaml) as the complete settings
reference. Start the service with `run --config /config/config.yaml`.
Configuration is loaded at startup; restart the container after changing it.

## Credentials and Overrides

Set `sonarr.url` and exactly one of `sonarr.api_key` or `sonarr.api_key_file`.
The example reads the key from `/config/sonarr_api_key`; keep that file private
and readable by the container identity. Never commit real credentials.

Precedence is built-in defaults, YAML, then `SRA_` environment variables.
Nested settings use double underscores:

```sh
SRA_SONARR__URL=http://sonarr:8989
SRA_RECOVERY__WAIT=30m
SRA_LOGGING__LEVEL=debug
SRA_SCOPE__INCLUDE_SERIES_IDS='[10, 11]'
```

Use `true`/`false` for booleans, Go durations such as `30m` or `6h`, and YAML
sequences for lists. Unknown keys, invalid values, unreadable inputs, and
conflicting credential sources fail startup. Switching to an inline API key
also requires removing the configured key-file source.

## Enable Recovery Deliberately

The default `dry_run: true` observes and records recommendations without
submitting automatic mutations. Import recovery is enabled but remains subject
to dry-run. Review `status`, `issues`, and `actions` before setting
`dry_run: false` and restarting. Startup logs and status expose effective permissions.

Cleanup, blocklisting, and replacement search default off:

- `cleanup.enabled` permits removal from the download client, potentially deleting
  all downloaded data for a shared download. There is no queue-only mode.
- `cleanup.blocklist` and `cleanup.replacement_search` are separate permissions;
  either requires cleanup to be enabled.
- Those additional permissions do not apply to generic Sonarr-rejected cleanup.

Read [policy](POLICY.md) before enabling cleanup. A permanent episode/folder
mismatch or explicit executable-file queue warning can qualify on the first
eligible scan, without `cleanup.wait`. Executable warnings need no media preview.
Manual `trigger --execute` can authorize one live cycle while background dry-run
remains enabled; it does not enable otherwise disabled cleanup permissions.

## Scope and Limits

Scope filters use positive Sonarr series IDs and exact queue download-client
names. Exclusions take precedence. Empty allowlists allow all otherwise eligible
work. Unknown series are checked after Sonarr matches them.

Default recovery wait is 15 minutes; cleanup wait is six hours. These are
observed waits, not time since Sonarr added a queue item. Restarts and failed
observation gaps do not advance them. See [policy](POLICY.md) for exceptions.

Imports, removals, and searches share limits of one mutation per scan and five
per rolling hour by default. Episode cooldown defaults to 30 minutes. Recovery
allows three attempts with a 15-minute retry interval; uncertain outcomes are
protected rather than blindly retried.

## State and Logging

`state_path` defaults to `/state/remediator.db`; preserve its private directory
across deployments. Local inspection commands use `--state PATH` when you change
this path. They do not load the service configuration to discover it.

Logs default to JSON at INFO on stderr. Use `logging.format: text` for text or
`logging.level: debug` for detailed decisions. Finished history defaults to
720 hours (30 days); records needed for unresolved effects, retry protection,
budgets, cooldowns, and continuations are retained. See [operations](OPERATIONS.md)
for inspection, log cadence and backup procedures.
