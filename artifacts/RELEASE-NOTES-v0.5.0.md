# v0.5.0

## Breaking cleanup behavior

When cleanup is enabled, Sonarr's explicit `Caution: Found executable file`
queue warning selects immediate removal of the entire shared download from
Sonarr and its download client. This can delete downloaded data on the first
eligible scan, without the configured cleanup wait, including when Sonarr's
manual-import preview is empty. Existing library files are preserved.

This addresses executable-containing downloads that previously remained blocked
because no importable media appeared in the preview. Release names, `.exe`
filenames, and unrelated warnings do not independently authorize deletion.

Cleanup permission, dry-run mode, client/series scope, protected tracked states,
competition, episode cooldowns, mutation limits, and pending/uncertain-action
protection remain in force. Immediate means no policy wait, not an out-of-band
mutation: the normal polling cycle and safety gates still apply. This path never
blocklists or launches replacement searches, even if those permissions are on.
Manual previews report zero cleanup wait without requiring `--override-wait`.

## Upgrade

Existing deployments require only an image-version update. No new environment
variables, mounts, commands, container permissions, or database migration are
required. Preserve the existing configuration and private state directory.
Disable cleanup before upgrading if automatic executable-download deletion is
unwanted. Follow the stopped whole-state backup and uncertainty reconciliation
procedure in `docs/UPGRADING.md`.

Publication and production deployment are separate operations; preparing this
release does not delete the currently blocked production download.

## Preparation evidence

- Focused executable-warning regressions pass, including first-observation
  removal, empty previews, cleared-warning rechecks, scope/permissions, required
  read failures, mutation budgets, cooldowns and uncertainty across restart.
- `mise run check` passes: formatting, vet, race-enabled Go suite, 33 Python
  checks, amd64/arm64 vulnerability analysis, Renovate validation and actionlint.
- The unchanged v0.4.1 baseline reports no selectable action for the isolated
  executable-warning/empty-preview scenario. The new CLI reports zero wait,
  preserves background dry-run isolation and verifies one explicitly executed
  removal with no wait override.
- Native rootless arm64 container acceptance passes, including automatic
  executable-warning cleanup with a six-hour wait configured, empty preview,
  verified queue absence and preservation of the existing library file. Local
  image config: `sha256:360bf06a667c1f6038bdec08829752f40787e7477aa91fb146d4226240abb6a8`.
- Release promotion requires the new executable-warning acceptance check.
  Native amd64 runtime acceptance and published-image verification remain CI
  release gates; they have not been performed locally.

This evidence describes the local worktree candidate, not a published release.
