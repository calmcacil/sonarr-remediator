# Manual Trigger Technical Specification

Status: implemented for target release 0.2.0. Implements
[PRODUCT.md](PRODUCT.md). Publication remains a separate operator action.

## Context

Research baseline: `b98f6be6c31b48f5e16c83353bf6fe6fbbefa6ca` (v0.1.1).
The baseline had a single observer, exclusive state ownership, immutable configured
dry-run at its Sonarr client boundary, and no manual control transport.

- [CLI dispatch](https://github.com/calmcacil/sonarr-remediator/blob/b98f6be6c31b48f5e16c83353bf6fe6fbbefa6ca/cmd/sonarr-remediator/main.go#L30) owns configuration, client/store creation and signal handling. Existing query commands open state read-only.
- [Serial observer loop](https://github.com/calmcacil/sonarr-remediator/blob/b98f6be6c31b48f5e16c83353bf6fe6fbbefa6ca/internal/service/service.go#L32) calls `Cycle` and waits on a timer. It cannot currently wake for a CLI request.
- [Initial waits and submission](https://github.com/calmcacil/sonarr-remediator/blob/b98f6be6c31b48f5e16c83353bf6fe6fbbefa6ca/internal/service/service.go#L245) check elapsed eligibility, shared gates, configured dry-run and per-cycle limits.
- [Fresh execution](https://github.com/calmcacil/sonarr-remediator/blob/b98f6be6c31b48f5e16c83353bf6fe6fbbefa6ca/internal/service/service.go#L477) refreshes evidence and persists intent before calling Sonarr.
- [Client mutation boundary](https://github.com/calmcacil/sonarr-remediator/blob/b98f6be6c31b48f5e16c83353bf6fe6fbbefa6ca/internal/sonarr/client.go#L49) rejects non-GET calls when its client is dry-run. Changing a display/config flag alone cannot authorize a live manual request.
- [Strict state loader](https://github.com/calmcacil/sonarr-remediator/blob/b98f6be6c31b48f5e16c83353bf6fe6fbbefa6ca/internal/state/state.go#L185) rejects unknown record kinds and requires exactly four records. A fifth receipt record cannot be added compatibly under schema 1.
- [Outcome verification](https://github.com/calmcacil/sonarr-remediator/blob/b98f6be6c31b48f5e16c83353bf6fe6fbbefa6ca/internal/service/verify.go#L16) reconciles pending effects independently of configured dry-run; preserve its v0.1.1 timestamp-precision correction.

## Implementation

### CLI and Local Control

Add `trigger` and the read-only `triggers` query to `cmd/sonarr-remediator`.
Use command-specific flags so unsupported options cannot be silently ignored.
Trigger flags are `--dry-run`, `--execute`, `--override-wait`, repeatable
`--download-id`, `--json`, `--timeout` and existing `--state` for local discovery.
Reject `--config` or caller-supplied policy/credential settings for a trigger.
`triggers` supports `--request-id`, `--limit`, `--json` and `--state`.

Introduce a small `internal/control` package used by the CLI client and observer
server. Use standard-library Unix-domain sockets and framed JSON, not HTTP/TCP,
new dependencies, shell commands, engine APIs or a second state writer.

Derive a deterministic socket filename from the configured state's absolute path
inside its existing private parent directory. No new configured control path or
mount is required. The server holds the existing store-owner lock before binding,
uses a mode-0600 socket in the mode-0700 directory, and verifies Linux peer UID
against its application UID. The response identifies the expected state/instance
without exporting credentials. The CLI verifies endpoint ownership and identity.

Socket creation/removal uses `Lstat`: never unlink a symlink, regular file, foreign
socket or live listener. Only an owned, demonstrably stale socket may be replaced
after acquiring store ownership. Close and remove only the server's own socket on
shutdown. An unsupported filesystem/path length, collision or bind/permission
failure disables manual control with a visible status reason; it does not make
existing background deployments require new mounts or different paths.

Validate protocol version, mode, booleans, selectors, time bounds and unknown fields
at both endpoints. Preserve download IDs as exact opaque strings; reject empty
selectors and deduplicate repeats without rewriting IDs. Bound a request to 16 KiB,
responses to 16 MiB and a selection to 1,000 downloads. A larger default selection
must fail before mutation and request narrower explicit IDs; do not truncate a
plan and execute unseen items. Bound decoding time and concurrent connections.
Check plan/report size before live submission; an oversized report must not hide
an already-sent effect. Runtime/result errors still return a bounded error report.

### Serialized Request Ownership

The service loop remains the sole owner of `Store.Data`, policy state, budgets,
attempts and Sonarr work. The control server validates a request, passes an owned
value plus cancellable context/result channel to the loop, and never reads or
mutates those structures from a handler goroutine.

Use an admission slot for one manual request total and a bounded channel to wait
for a current automatic cycle. Other callers receive `busy`; there is no durable
execution queue. Keep periodic scheduling independent of requests: service an
overdue background cycle before admitting another manual cycle. Do not reset the
poll timer after every trigger or allow a stream of previews to starve observation.

Each socket/parser goroutine has an explicit service owner, deadline, close path
and join. Default/end-to-end request timeout is 120 seconds, with the existing
50-second cycle deadline capped by remaining request time. Propagate cancellation
through dispatch, reads and submission. A disconnect cancels unstarted work and
prevents another mutation; accepted effects retain ordinary uncertain/pending
protection. Stop listener/admission first during shutdown, cancel active I/O, join
handlers and close state within the existing application/engine stop allowance.

### Shared Planning and Execution

Refactor `Cycle` into common observation/planning helpers and an explicit
cycle-local execution context. Keep the public background path as its configured
default. Proposed options contain mode (background/preview/manual-live), exact
target selection, wait override and request identifier. Do not mutate `Service.Config`,
the background client's `dryRun`, or global permissions temporarily.

Build a per-cycle Sonarr client with a hard dry-run boundary for preview; an
explicit manual-live request gets a live client while retaining every configured
policy permission. Its construction uses only the daemon's already-loaded
configuration. Cache required resources only within that cycle as today.

Share the following sequence between preview and execution:

1. Read/validate Sonarr version and the complete queue. Group by download ID;
   include selected pending-search continuations even if absent from the queue.
2. Observe required previews, episode/files, profiles, scope and grabbed history.
   Use the complete queue for competition. Required-read failure stops submission
   and never becomes an empty queue or approving fallback.
3. Reconcile outstanding effects using reads. Preview performs this and all other
   projections on a deep isolated view, not maps/slices aliased to live state.
4. Evaluate policy, then initial waiting, then existing pending/retry/cooldown/hour
   gates. Skip only the initial wait comparison when explicitly requested.
   Do not set `Elapsed` to the threshold, rewrite due timestamps to pretend age,
   or change normal waiting configuration.
5. Sort already-due candidates by the existing due key; sort override-only work
   by first observed eligibility, then download ID. A newly observed eligible
   candidate uses the current observation as its first-seen ordering key.
6. Allocate per-cycle and remaining hourly slots virtually for preview or actually
   for live work. Count each mutating HTTP request, including continuation search.
   Return policy eligibility separately from selection/limit reasons.
7. Live execution repeats the existing fresh checks before durable reservation
   and transmission. Restrict new submissions/continuations to selected downloads.
   Permission changes or differing candidate fingerprints defer rather than reuse
   an old preview. Persist each effect before and after its network call as today.

Preview must not call writable-store `Save`, persist recommendations, change
`previous` observation time, or mutate live status/attempts/operations. Use immutable
input snapshots/deep copies and a result DTO; do not repurpose normal dry-run
`Cycle`, which currently writes observations and recommendations.

Manual-live reads can contribute only genuinely observed eligibility progress
through the common rules. Any elapsed-time credit is independent of override.
Status continues to describe the background mode; the manual report/receipt
describes its one-off mode and permissions separately.

### Receipts, State Compatibility and Failure Recovery

Add schema 2 with a fifth `manual_triggers` record. A typed receipt contains request
ID, accepted input/selection, effective permissions/limits, wait-override marker,
submission state, selected/blocked summary and linked existing attempt IDs.
Add optional manual source/request/override audit fields to `state.Attempt`.
Keep attempt outcomes, intent-before-transmission, shared budgets and verification
as the authority for effects; derive receipt outcome from linked attempt records
instead of maintaining conflicting success flags.

Migrate schema 1 only after the existing exclusive lock, full record validation and
instance-binding check. In one SQLite transaction, insert the empty receipt record
and advance `user_version` to 2 without resetting the original four records. A
failed/interrupted migration leaves schema 1 intact and sends no mutations.
Fresh stores use schema 2. Read-only v0.2.0 queries can read schema 1 without
creating/upgrading it; `triggers` then has no manual receipts. Schema-2 loaders
strictly validate all five records and retain unknown/corrupt-state rejection.

Generate request IDs with `crypto/rand` in the CLI; return/print the identifier
before a live request may be sent, including JSON-compatible error reporting.
Use issued/expiry times bounded by the request timeout to reject stale unaccepted
wire messages. Persist an accepted receipt before any submission; add its linked
attempt ID in the same transaction as durable mutation intent. The same accepted
ID and payload returns the receipt, never a second action; a conflicting payload
fails. No automatic client resend follows an ambiguous socket write/response.

On restart, receipts without an attempted effect are interrupted/cancelled, not
executable work. Linked intents retain existing uncertain/read-reconciliation
rules. Pending effects can verify while background mode is dry-run; permission
checks and active mode still govern later search submission. A manual request does
not retain execution permission or wait override for future automatic work.

Finished receipts follow existing 30-day retention, but retain those still needed
by unresolved attempts/continuations. Prune linked attempt/receipt references
consistently; stale unaccepted wire messages cannot resurrect pruned requests.

State compatibility is deliberately explicit: v0.1.x refuses schema 2. Release
notes must call out that state-format compatibility break and require private
stopped-state backups under the existing update procedure. Image downgrade alone
cannot restore an older store. Never restore pre-trigger state over effects that
were subsequently accepted by Sonarr. Forward deployment needs only the image
update and automatic migration; any manual config/mount/lifecycle prerequisite
would be a breaking deployment change under AGENTS.md and requires spec revision.

### Reports and Documentation

Define a stable JSON report schema with request mode/ID, effective flags, selector,
queue-row/shared-download counts, budgets, item policy action, disposition,
affected IDs/file count, initial wait/progress/override, blockers and attempt
outcomes. Do not export API keys, raw environments or credential-bearing URLs.
Take preview-file count from the raw validated observation, not `Decision.Files`,
which can be empty after an early deferral. Carry explicit preview/membership
blocker details from observation into the report without weakening policy.
JSON stdout is one document; diagnostics and the recoverable request ID on lost
responses use stderr. Read-only `triggers` derives current outcomes from state.

Implement the product exit-code contract separately from existing query/health
commands. Acknowledged submission may exit successfully while its effect remains
pending. Empty/blocked/limited results are distinguishable from read failures.

Update README and OPERATIONS with proposed-to-real commands, one-cycle limits,
private control, one-off active mode, receipt lookup and backup/rollback behavior.
Keep automatic defaults unchanged. Align base PRODUCT/TECH during implementation
for the explicit manual exceptions and local control boundary. Change VERSION and
image examples to 0.2.0 only in the completed implementation/release PR, through
the existing tested-image promotion workflow; do not publish this draft.

## Testing and Validation

Use synthetic Sonarr fixtures/identities/paths/dates/scores. Specs-first drafting
uses source review; implementation must satisfy the following before publication.

| Product invariants | Evidence required |
| --- | --- |
| 1-7, 31-33 | CLI parser/default/conflict tests; real same-container IPC; stopped/busy/incompatible daemon; exact target grouping and no selector-based competition bypass |
| 2, 17, 19 | Capture GET-only preview, byte-for-byte unchanged state records/progress/budgets, isolated maps/slices, budget-aware ordering, and live reevaluation after a changed preview |
| 3, 8-15, 23 | Manual execution while background dry-run stays true; initial-wait-only bypass; first eligible observation; disabled policies, cooldown, retry/exhaustion and uncertain work still blocked; search continuation scope |
| 16-22 | Multi-target plan with one selected default request; shared hour budget across manual/automatic imports/removals/search; coupled client deletion/blocklist output; acknowledged versus correlated outcomes |
| 24-28 | Deterministic barriers for background/manual serialization, admission busy, timer fairness, queued/active cancellation, lost response, duplicate IDs and crash before/after transmission; no replay on restart |
| 26-30, 34 | Atomic schema migration failure injection and integrity checks; all prior history retained; receipt/attempt transaction ordering; dry-run reconciliation; retention, older-reader rejection, JSON shape and exact exit codes |
| 29, 31-34 | Secret-redaction tests; wrong peer UID, unsafe/stale socket handling, oversized/trailing/unknown protocol data, custom/unsupported state path and safe control-unavailable reporting |

Run focused packages and `mise run check`, including race detection. Extend native
rootless Podman amd64/arm64 acceptance to use the existing state/config mounts,
read-only root and UID mapping with no new ports/capabilities. Exercise preview,
one-off active wait bypass on fake Sonarr, receipt queries, persisted pending
reconciliation, cancellation and bounded shutdown in the actual image. Check the
secondary rootless Docker recipe; no production queue mutation is needed for CI.

Review generated examples/Quadlet behavior for unchanged startup, health command,
volume paths, identities, networks and stop budgets. Any required deployment diff
beyond selecting the new image must block the planned compatibility claim.

## Implementation Order and Parallelization

Implement sequentially in one PR: common cycle/preview
planning, schema/receipt support, serialized control/CLI, then container acceptance
and operator docs. The work is tightly coupled at the state/ownership boundary;
parallel agents are not proposed. Keep both specs aligned with the implementation.
Publishing a release is separate from implementing and validating the feature.

## Local Verification

Verified on 2026-10-12 against the implementation worktree with the project-pinned
mise tools. All Sonarr interactions used synthetic fixtures.

- `mise run check` passed: formatting, vet, the Go race suite, 33 Python tests,
  Renovate validation, Actions lint and reachable-vulnerability checks for Linux
  amd64/arm64. No reachable vulnerabilities were reported.
- Manual regressions cover isolated preview state, default waits and first-observation
  override, disabled permissions, protected states, cooldown/retry/exhaustion,
  complete-queue competition, virtual budgets, mixed acknowledged/uncertain effects,
  newly reconciled search continuations, admission/cancellation, duplicate requests,
  migration failure, restart protection and receipt/attempt retention.
- A five-second `FuzzRequestFrame` run passed 153,377 executions. Socket tests reject
  unknown/duplicate authorization fields, unsafe path collisions and oversized data,
  and exercise disconnect cancellation and owned stale-socket replacement.
- `mise run build` produced `bin/sonarr-remediator`; `version` reports `0.2.0`.
  A separate static Linux amd64 cross-build also passed.
- Native rootless Podman 5.4.2 on arm64 passed all 18 acceptance checks with the
  existing mounts, private UID mapping, read-only root and no published ports.
  Manual preview sent no mutation; one-off live wait bypass imported the fixture;
  background dry-run verified the import; receipts survived restart; the manual
  timeout and exit-code checks passed. Shutdown took 0.204 seconds.
  Local runtime evidence is in ignored `artifacts/manual-trigger-runtime.json`.
  The promotion validator requires all 18 named checks; a regression prevents
  drift between the harness and the receipt contract.
- `docker compose config --quiet` passed. No rootless Docker daemon is available
  on this host, so Docker runtime acceptance was not performed. Native amd64
  container acceptance and committed-source release evidence remain CI checks
  before publication.

The schema-2 upgrade and v0.1.x downgrade incompatibility are documented in
`docs/OPERATIONS.md`. Deployment startup, healthcheck, mounts, permissions and
container identity remain unchanged apart from selecting the 0.2.0 image.
