# Sonarr Remediator Technical Specification

Status: approved by the operator on 2026-10-12, alongside PRODUCT.md. This is
the implementation plan for the clean rewrite.

## Context

[PRODUCT.md](PRODUCT.md) defines operator behavior. The empty rewrite branch
starts at `862bec0`; original code at
`71566c26107e8410d12489e55481f061e6bc3479` is historical context rather than
an architecture to preserve. [SOURCES.md](SOURCES.md) records recovered specs.

Agreed technical baselines are Go, one Sonarr v4 instance, local SQLite state,
rootless Podman as primary build/runtime, rootless Docker as secondary
compatibility, and no inbound server or direct media/download-client access.
Go's HTTP, JSON, context, logging, and testing packages cover the core work.
Rust would also fit, but is not needed to meet these requirements.

The initial service implementation is now present in this checkout. The pinned
toolchain is Go 1.27.2; SQLite and YAML dependencies are locked in `go.mod` and
`go.sum`. [VALIDATION.md](../../docs/VALIDATION.md) records the actual verification
scope, independent review, and platform acceptance still required before release.

[API evidence](evidence/sonarr-v4.0.20/README.md) contains anonymized live
responses and a [browser trace](evidence/sonarr-v4.0.20/BROWSER_FLOW.md). The
running version `4.0.20.3014` corresponds to upstream commit
`cab419ade8ac7fcab5bf80394ee492abd35d5f5a`; source findings below are pinned
to that version.

## Proposed Changes

### Program and Ownership

Ship one binary with `run`, `status`, `issues`, `show`, `actions`, `healthcheck`,
and `version` commands. Running the binary without a subcommand starts `run`.
Local queries accept `--state` and `--json`; `healthcheck` returns an exit code.

Keep five internal packages: `config`, `sonarr`, `policy`, `state`, and `service`.
The command entry point owns wiring and output formatting. `policy` accepts
typed observations and returns a proposed action or explicit blocking reasons;
it performs no I/O. `service` owns one serial scan/action loop and calls the
other packages. There are no detector plugins, score-based confidence system,
parallel workers, timer per issue, generic job queue, or compensating actions.

Each cycle reads the complete queue, updates observations, verifies outstanding
attempts, and evaluates due candidates. One permitted mutation can be sent
after fresh checks. Candidate order is oldest due work first with a stable
identity tie-break; a candidate waiting for retry or budget does not prevent
read-only evaluation of others. No overlapping cycles or catch-up bursts.

Use narrow Sonarr DTOs with presence-aware fields for required safety data.
Absent/null identity, timestamp, score, and verdict fields must not silently
become an approving zero value. Stable local identity includes the configured
Sonarr identity and its opaque download ID; queue IDs remain transient row
references. Store the Sonarr identity in state and reject accidental reuse
against another instance until the operator explicitly chooses a new store.

### API Contract

All routes are under `/api/v3`, including when the running application is v4.
Preserve a configured URL base path. Use `X-Api-Key`, a 10-second request timeout
by default, normal TLS verification, no cross-origin credential redirects, and
bounded response reads. Startup rejects a successfully identified non-v4
instance; an offline instance leaves the initialized service waiting/degraded.

| Endpoint | Purpose |
| --- | --- |
| `GET system/status` | Detect application version and connectivity |
| `GET queue` | All pages with `includeUnknownSeriesItems=true` |
| `GET manualimport` | `downloadId=<Sonarr ID>&filterExistingFiles=false`; automatic browser-equivalent matching |
| `GET episode/{id}`, `GET episodefile/{id}` | Current library identity and existing-file state |
| `GET series/{id}`, `GET qualityprofile/{id}` | Monitored state and current applicable profile |
| `GET config/mediamanagement` | Proper/repack preference; decode only the required field |
| `GET history` | Exact `downloadId` filtering; grabbed membership and correlated outcomes |
| `GET command/{id}` | Track the command accepted for this attempt |
| `GET blocklist` | Confirm requested blocklisting when correlatable |
| `POST command` | `ManualImport` or `EpisodeSearch`, after policy approval |
| `DELETE queue/{id}` | Client removal, optional blocklist, explicit suppression of implicit search |

For queue reads, request 100 records/page; validate page metadata, positive row
IDs, no duplicate IDs, and complete reported totals. Restart changing reads at
most twice, with 1,000 pages per attempt. This cannot create an atomic snapshot;
immediately refresh relevant state before each mutation. Cache supplementary
resources only within a cycle, including failed reads. No unbounded global
history fetch per episode or full library scan.

History is filtered by the actual Sonarr download ID and read newest-first,
100 records/page, at most 100 pages. Intended membership requires complete
grabbed history when queue/preview cannot establish a pack's scope. Verification
can stop at an attempt-time cutoff once all needed outcomes are established.
Incomplete history never proves missing activity, complete membership, or
success. The fixture contains no history; add controlled history fixtures
during implementation instead of inferring it from the queue.

Successful JSON is limited to 16 MiB per response with overflow detection;
error snippets are limited to 64 KiB and sanitized before logging. Required
resources reject empty/null/malformed JSON and mismatched IDs. Mutation routes
may return their documented empty replies. Unknown enums defer the affected
policy. Use a 50-second cycle deadline by default; exhaustion preserves work
for a later cycle and does not publish partial observations as a full scan.

### Browser Matching and Upgrade Policy

The confirmed browser flow needs only the initial GET preview. Automatic
recovery uses Sonarr's returned matches without editing show, season, episode,
or quality. Do not use the evaluating `POST manualimport` endpoint or a browser
session in the initial implementation. Every mutating HTTP request is visible
at one guarded client boundary; dry-run cannot pass that boundary.

Copy the browser's tracked-download command shape, including path, folder name,
series ID, every episode ID, quality/revision, languages, release group,
indexer flags, release type, and download ID, with `importMode: auto`.
Submit nothing if any selected file fails the shared policy.

Strict replacement approval requires both a zero-rejection current preview and
an explicit improvement permitted by the current profile. Implement a small
profile-comparison helper grounded in Sonarr's
[UpgradableSpecification](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/DecisionEngine/Specifications/UpgradableSpecification.cs),
[QualityModelComparer](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/Qualities/QualityModelComparer.cs),
and [import upgrade checks](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/MediaFiles/EpisodeImport/Specifications/UpgradeSpecification.cs).
Use profile item/group order, `upgradeAllowed`, quality/custom-format cutoffs,
`minFormatScore`, `minUpgradeFormatScore`, and proper/repack preference. Grouped
qualities are equivalent for this comparison. Revisions and custom-format
scores are compared in the applicable Sonarr policy branch, not as an invented
weighted sum. Reject unsupported profile shapes and missing information.

The stricter service gate never replaces an equal candidate. Any deliberately
more conservative behavior than Sonarr's interactive override must be stated
in policy output. In particular, an accepted preview alone does not prove a
strict improvement: the captured first browser example has equal current
quality/revision and score despite zero rejections.

The service additionally requires `upgradeAllowed` for revision replacements,
requires `preferAndUpgrade` for revision-only improvement, and does not treat a
revision increase across two grouped-equivalent quality IDs as a revision-only
upgrade. Policy output states these conservative deferrals. Format scores are
recomputed against the current profile and must agree with returned scores;
unknown formats and stale scores defer action.

### Cleanup and Outcome Verification

Classify only a fixed, source-verified catalog of reasons. Initial unusable
reasons are exactly `Sample` and `No audio tracks detected`.
`Unable to determine if file is a sample`, TheXEM mapping, access errors, empty
previews, and unknown wording are preserved. Rejection `type=permanent` is not
a deletion category. Redundancy additionally requires complete intended scope
and acceptable current files for every intended episode; the two captured
multi-file cases fail that requirement.

Sonarr's [queue controller](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/Sonarr.Api.V3/Queue/QueueController.cs)
defines independent parameters. Always send them explicitly:

```text
removeFromClient=true
blocklist=<policy-approved true or false>
skipRedownload=true
changeCategory=false
```

One DELETE may remove client data and blocklist a release. The request counts
as one mutation attempt, with both effects recorded. Redundancy always uses
`blocklist=false`. Do not add a later `history/failed` call, which can invoke
additional effects. Use a separate `EpisodeSearch` command only after the
allowed removal is verified and its targets are still eligible.

Record the attempt and reserve its budget before sending. Verification uses
later serial cycles, never a blocking background timer. Track command state,
download-correlated history, and current episode-file state. For import, require
all intended files/episodes to be accounted for; command success or vanished
queue rows alone is insufficient. For deletion, require Sonarr acknowledgement
and complete queue absence; this verifies Sonarr's removal contract, not an
independent inspection of client disk. If blocklisting is requested, also
require unambiguous corresponding failed/blocklist evidence before marking the
whole operation complete. Source shows blocklist responses have no download-ID
field, so title/series/episode/date correlation must be unambiguous.

Sonarr history and blocklist timestamps may have whole-second precision. For
those events only, truncate the attempt's lower time bound to the same second;
retain exact lower bounds for events with fractional seconds. Reject events in
earlier seconds and events after the current scan. This time comparison does
not replace the required identity, path, changed-file and current-profile checks.
Preserved pending attempts can reconcile during dry-run without resubmission.

Verification has a 10-minute deadline. Timeout is uncertain, not a
retryable failure. Continue read-only reconciliation later; never automatically
replay an ambiguous command or deletion. Known command failure can use the
configured recovery retry schedule; partial import or partial cleanup stays
visible and protects remaining work. A search command acknowledgement is
submission evidence only, not proof that new media will be downloaded.

### SQLite State

Use `database/sql` and the pinned pure-Go SQLite driver
`modernc.org/sqlite`, to retain a static `CGO_ENABLED=0` binary. Use a private
local volume, WAL, full synchronous durability, transactional schema versions,
and an exclusive process lock for `run`; local queries open the existing store
read-only. A state initialization/migration failure starts no monitors.

Four logical records suffice: service status/identity, download observations,
action attempts, and operation progress. Episode cooldowns and rolling budgets
are derived from retained attempts, rather than separate scheduling tables.
Store eligible elapsed time, candidate fingerprints, attempt count/next due
time, before-state/expected identities, command IDs, and verified/uncertain
effects. Persist intent and budget atomically before network I/O. A crash
between intent and response yields uncertainty requiring read-back; it cannot
prove that the request was not sent.

Schema 1 stores the four logical records as JSON bodies in a SQLite `records`
table. Required records cannot be missing or null when reopening an initialized
store. Due timestamps are persisted for stable scheduling. Partial imported
episode effects are monotonic historical evidence; complete success still needs
fresh acceptable current files for all targets. Read errors do not bypass the
verification deadline or erase an already established effect.

Accrue waiting time only between successful eligible observations. Credit at
most one polling interval per pair; do not credit the interval across process
restart or a failed/gapped scan. Persist accrued progress; reset when an
ineligible state is actually observed. Fingerprints include file identities,
target episodes, quality/revision, custom formats, and material verdict changes;
timestamps or mere response ordering cannot reset retry protection.

Bind pending effects and cooldowns to the download and every affected episode.
The continuation search is part of its removal operation and can bypass only
that operation's successful-removal cooldown, never another operation's lock
or cooldown. Prune finished records after 30 days unless needed by current
safety state. Any write failure stops further mutations; recovery first
reconciles outstanding attempts.

### Configuration and Operator Commands

Use a strict YAML decoder and `SRA_` environment overrides with double
underscores for nesting. Start a new schema rather than accepting inert legacy
settings. Approved defaults and configuration names are:

```yaml
sonarr:
  url: http://sonarr:8989
  api_key_file: /run/secrets/sonarr_api_key
  timeout: 10s
dry_run: true
poll_interval: 60s
state_path: /state/remediator.db
recovery:
  enabled: true
  wait: 15m
  max_attempts: 3
  retry_interval: 15m
cleanup:
  enabled: false
  wait: 6h
  blocklist: false
  replacement_search: false
limits:
  mutations_per_scan: 1
  mutations_per_hour: 5
  episode_cooldown: 30m
scope:
  include_series_ids: []
  exclude_series_ids: []
  include_download_clients: []
  exclude_download_clients: []
logging:
  level: info
  format: json
history_retention: 720h
```

`sonarr.api_key` is an alternative to `api_key_file`; configuring both fails.
An environment override such as `SRA_SONARR__URL` replaces the YAML value.
Client scope uses exact queue client names: the captured queue has a client
name but no client ID. No download-client configuration or credential fetch is
required. Validate positive intervals/counts, cleanup wait not shorter than
recovery wait, valid scope IDs/names, and cleanup prerequisites.

`status` shows mode, effective permissions, connectivity, scan recency, current
issue counts, budgets, and unresolved outcomes. `issues` lists current work;
`show` explains one download; `actions` lists recent recommendations/attempts.
Support result limits and a `--download-id` filter for actions. Missing state,
unknown identifiers, and unsupported schema return nonzero exit codes without
creating a new store. These commands need no Sonarr credentials.

`healthcheck` reads persisted status: local readiness, a heartbeat and complete
scan no older than three poll intervals, and no unresolved required-read/state
failure. Expected policy rejections or exhausted mutation budgets are not
health failures. Structured logging uses `log/slog` on stderr; command output
uses stdout. Redact keys, credentials, and URL userinfo; deduplicate unchanged
decisions without hiding new errors.

### Container and Delivery

Build with a pinned supported Go toolchain and dependency lock data. Produce
Linux amd64/arm64 static binaries and a minimal runtime image with CA roots,
non-root UID/GID 65532, no shell dependency, and exec-form entry point. The image
contains an appropriately owned `/state` directory for volume initialization;
config and secret files are mounted read-only. Use a local named volume or a
correctly mapped bind mount; a rootless daemon's UID namespace is accounted for
explicitly rather than assuming host UID equals container UID.

Use one OCI-compatible `Containerfile`, built with native rootless Podman.
Provide a primary rootless Podman run recipe and a secondary Docker Compose
recipe. Both use a read-only root, all capabilities dropped, no privilege
escalation, no published ports, a local health command, and a 30-second stop
allowance. Docker builds explicitly select `-f Containerfile`; the Compose
recipe sets `read_only: true` and `stop_grace_period: 30s`.
The application stops within 25 seconds, recording
unverified in-flight work. Provide permission, state backup, restore, and
rollback instructions. No production deployment or update automation is part
of this rewrite's first implementation.

## Testing and Validation

No live mutation is needed or permitted by the current evidence-gathering
authorization. Implement validation with a fake Sonarr service and disposable
rootless containers; real active acceptance is a separately authorized step.

| Product invariants | Required proof during implementation |
| --- | --- |
| 1-7 | Strict config/secret precedence, permission independence, exclusions, sole Sonarr destination, dry-run mutation trap |
| 8-14 | Complete/empty/changing pagination, absent timestamps, protected tracked states, shared IDs, failed scans, persisted waits |
| 15-22 | Captured browser/API case matrix, strict upgrade/profile grouping/revisions, all rejections, missing intended episodes, pack atomic policy, fresh execution checks |
| 23-24 | Three-attempt bound, 15-minute spacing, no blind retry, correlated file/history verification, partial and external outcomes |
| 25-30 | Six-hour cleanup, exact unusable catalog, redundancy proof, explicit DELETE flags, no implicit search, partial effect and search continuation |
| 31-36 | Mutation caps and cooldowns across restart, dry-run separation, duplicate process lock, state write failure, crash-before/after transmission |
| 37-43 | Local query/JSON output, redaction, health distinctions, retention, native rootless Podman permissions/persistence/shutdown on both platforms, secondary Docker recipe |

Use the evidence sections as fixtures, with explicit adapters for their curated
field projections. Add synthetic variations for scenarios not observed: active
downloads, valid multi-episode imports, timestamp gaps, unknown enums, bad
profiles, new exclusions, API errors, command lifecycle, history correlation,
ambiguous blocklist evidence, and multi-step failures. Never label the accepted
equal browser example as a strict upgrade fixture.

Run focused Go unit/integration checks, `go test -race ./...`, `go vet ./...`,
and actual rootless Podman smoke checks for config/state permissions, no inbound
port or media mount, persistence, dry-run, health, and bounded shutdown. Build
and verify both platform images; do not broaden claimed platform acceptance
from one architecture's results. Rootless Docker compatibility checks are
secondary and are not an equally required PR gate. Release only the exact
immutable image produced and validated in PR CI, as specified in
[the Actions technical plan](../github-actions/TECH.md); promotion does not
rebuild it or repeat the CI suite.

## Implementation Order

1. Use the approved PRODUCT.md and TECH.md as the implementation baseline.
2. Build config, strict read-only Sonarr client, state, and local query commands;
   validate against the captured fixture shapes.
3. Add the pure policy and dry-run scan loop; verify the evidence case matrix.
4. Add guarded import execution, budgets, retries, and outcome verification.
5. Add separately enabled cleanup/blocklist/search with partial-effect handling.
6. Package and validate rootless Podman, document secondary Docker compatibility,
   then prepare a
   reviewable release. Activating it against a live Sonarr is a later decision.

## Parallelization

Implement sequentially in `clean-rewrite`. The API, shared policy, and durable
attempt boundaries are small and tightly coupled; concurrent ownership would
add coordination before those contracts stabilize. No subagents or separate
implementation worktrees are proposed.

## Compatibility

Source pinning and fixtures establish a v4.0.20 contract. Future v4 changes
must retain conservative unknown-value behavior and add compatibility evidence.
Radarr will be a later adapter; the first implementation builds no framework
for additional applications.
