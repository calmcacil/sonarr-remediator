# Recovery and Cleanup Policy

The service handles completed Sonarr v4 downloads in `importBlocked`,
`importFailed`, or `importPending`. It leaves downloading, queued, paused,
`importing`, and `imported` work alone. Shared queue rows produce one decision
per download. Sonarr performs matching and all media operations.

## Recovery

Automatic recovery waits for 15 minutes of observed eligibility by default.
Restarts and failed observation gaps do not advance the wait. Old queue age does
not count as time observed by this service.

Every media file must pass policy before an import is requested. Rejected or
ambiguous files defer the whole pack. Imports require complete episode membership.
Existing files are replaced only for a strict improvement under Sonarr's current
quality profile: grouped qualities compare equally, and cutoffs, minimum
custom-format scores, and proper/repack preference apply. An accepted preview
with equal quality and score does not authorize replacement.

## Cleanup Is Destructive

Cleanup is disabled by default. When enabled, it asks Sonarr to remove the shared
download from the queue and download client. This can delete a whole pack's
**downloaded data**, even when only one file is rejected. Existing library files
are preserved. There is no queue-only cleanup mode.

| Cleanup case | Eligibility and effects |
| --- | --- |
| Persistent Sonarr rejection | A valid rejection persisting for `cleanup.wait` (default six hours) can authorize deletion, including indeterminate samples, mapping/access failures and unknown rejection wording. No complete pack-membership or independent upgrade proof is required. Never blocklists or searches. |
| Executable-file warning | Sonarr's explicit `Caution: Found executable file` queue status message authorizes removal on the first eligible scan, even with an empty import preview. Never blocklists or searches. |
| Permanent episode/folder mismatch | Sonarr's explicit permanent `Episode ... was unexpected considering the ... folder name` rejection, including its plural form, can authorize deletion on the first eligible scan without the cleanup wait. Never blocklists or searches. |
| Explicit unusable media | The separate `Sample` / `No audio tracks detected` path can blocklist or request replacements only with the corresponding permissions and full required evidence. |
| Redundant download | Equal or inferior candidates can be removed when acceptable library files exist for every intended episode. |

For generic rejected cleanup, a separate rejected-time clock starts with a valid
rejection and resets when the blocker clears. Earlier eligible age does not
prefill it. Downtime and failed reads do not advance it. Other rejection wording
and temporary episode mismatches retain the configured wait.

Empty previews alone do not authorize deletion; the explicit executable-file
warning is a separate authority that does not require a media preview. Malformed
or failed required reads remain protected. Scope, competition, cooldowns, mutation
budgets, and pending/uncertain-action protection still apply to immediate cleanup.

## Budgets and Uncertain Outcomes

By default, imports, removals, and searches share one mutation per scan and five
per rolling hour. Intent and budget are stored before transmission. Acknowledged
submission is not proof of external success; the observer checks correlated
Sonarr evidence afterward. Partial effects and unknown outcomes remain protected
across restarts and are never blindly retried.

Manual `--override-wait` skips recovery/cleanup waits and episode cooldowns for
that request. It does not bypass permissions, competition, retry spacing, attempt
exhaustion, shared budgets, protected states, or uncertain effects. See
[manual cycles](OPERATIONS.md#manual-cycles) before using it.

Use `show DOWNLOAD_ID` to understand a blocker and `actions` to distinguish
recommendations, acknowledgement, verification, partial effects, and uncertainty.
For unresolved work, follow [intervention guidance](OPERATIONS.md#intervention).
