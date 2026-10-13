# Log Summaries Technical Specification

Status: approved and implemented. Implements [PRODUCT.md](PRODUCT.md) for 0.4.1.

## Context

Research baseline: `e3ee7a313d653c6cb45df637ec738effacc68897` on `main`.

- [Decision logging](https://github.com/calmcacil/sonarr-remediator/blob/e3ee7a313d653c6cb45df637ec738effacc68897/internal/service/service.go#L464) uses INFO when state or reason changes. Waiting reasons contain elapsed time, so deduplication does not stop poll-by-poll output.
- [Service cycle](https://github.com/calmcacil/sonarr-remediator/blob/e3ee7a313d653c6cb45df637ec738effacc68897/internal/service/service.go#L123) owns observation, verification, plans and execution, including early returns on deferred reads.
- [Manual worker](https://github.com/calmcacil/sonarr-remediator/blob/e3ee7a313d653c6cb45df637ec738effacc68897/internal/service/manual.go#L84) copies `Service`; previews use a deep state projection while live execution shares state and transfers the observation clock back.
- [Existing aggregation](https://github.com/calmcacil/sonarr-remediator/blob/e3ee7a313d653c6cb45df637ec738effacc68897/internal/state/state.go#L359) counts retained issues including resolved history. Logs must filter resolved entries without changing the status API.
- [Logger configuration](https://github.com/calmcacil/sonarr-remediator/blob/e3ee7a313d653c6cb45df637ec738effacc68897/cmd/sonarr-remediator/main.go#L77) already handles levels and JSON/text output; the configuration key is `logging.level`.

## Proposed Changes

1. Change `setIssue` to DEBUG, retaining its change predicate and structured
   attributes. Do not change policy decisions or their durable state updates.

2. Add a small service-owned in-memory summary state and logging helper, preferably
   in `internal/service/logging.go`. Track the last summary time, counts of actual
   mutation calls by kind, counts of new dry-run recommendations, and observed
   outcome changes since the previous summary. Use `Service.Now` for deterministic
   interval checks. Keep telemetry out of SQLite and avoid additional goroutines,
   configuration, dependencies, or changes to the status/CLI schemas.

3. Capture outcome changes around real cycles, keyed by existing attempt IDs.
  Compare actual attempts before and after reconciliation/execution; ignore
  unchanged outcomes, unsent intents, preview projections, dry-run attempts, and
  startup history.
   Capture changes before pruning can remove newly completed history. Accumulate
   changed outcomes until a summary consumes them. Count each newly appended
   recommendation once, using the existing recommendation deduplication.

4. Count actual calls at the import/remove/search transmission boundary, after
   fresh checks and durable reservation. A failed reservation is not a mutation.
   Reuse the existing `mutation attempted` attributes for search, which currently
   lacks that record. Preserve successful verification records and warning paths.

5. At real cycle completion, including early deferred/error returns, collect
   telemetry and emit at most one summary when initial, due, or mutation-triggered.
   Use the completed cycle's current state and a current end-of-cycle timestamp.
   Do not describe a stored complete scan as current when the cycle was deferred.
   Emit action visibility even if a post-transmission save or later read fails.

6. Ensure the copied manual worker returns live summary state to its owning
   service after execution, including error paths. Preview workers must neither
   emit summaries nor change live counters/deadlines. Receipt replays skip cycles
   as today. Preserve single-loop ownership and existing automatic poll deadlines.

7. Log `service summary` with stable fields: `trigger` (`startup`, `interval`, or
   `action`, giving action priority), `dry_run`, `connectivity`, `complete_scan`,
   `active_issues`, `issue_counts`, `unresolved_actions`, `mutation_attempts`,
   `action_outcomes`, `dry_run_recommendations`, `mutations_last_hour`, and
   `hour_limit`. Counts cover the process-local interval since the previous
   summary. Use empty count maps/zeros for empty activity; encode maps consistently
   in both handlers and avoid unbounded download identifiers in summaries.

8. Update `docs/OPERATIONS.md` to describe cadence, immediate action logs, DEBUG,
   and restart behavior. Set `VERSION` to `0.4.1` and align the application fallback
   version in `cmd/sonarr-remediator/main.go`. Classify this as a compatible logging
   fix in release notes; no operator deployment migration is required.

## Testing And Validation

Use the existing service fake Sonarr, temporary SQLite stores, injected clock,
and actual `slog` handlers to validate emitted records. Apply the
`agent-change-verification` workflow with these observable checks:

- Behavior 1-2, 11: reproduce changing wait reasons across consecutive minute
  cycles; assert no INFO decisions, detailed DEBUG decisions, and threshold
  filtering in JSON and text. Temporarily restore INFO decision logging and
  prove the regression fails before restoring the fix.
- Behavior 3-4, 6, 8: cover startup, empty queue, 29:59 and 30:00 boundaries,
  repeated minute polls, a later due cycle, and a deferred observation with stale
  scan time. Assert no repeated summaries before the deadline.
- Behavior 5-7, 9: exercise real fake-server import/remove/search calls; compare
  mutation counts with received calls, coalesce multiple actions into one summary,
  reset the interval, and cover ambiguous results and a failed pre-submit gate.
  Verify unchanged pending outcomes are not counted repeatedly and later
  verified/failed/uncertain outcomes appear once in subsequent summaries.
- Behavior 7-8, 11: cover new versus deduplicated dry-run recommendations,
  restored historical attempts, resolved retained issues, and counter reset.
- Behavior 10: preview leaves logging cadence/counters intact; live manual
  execution triggers a shared summary and postpones the next periodic summary;
  a receipt replay produces no duplicate mutation summary.
- Behavior 12: review the diff for configuration/state/container contract changes;
  run focused service tests, then required `mise run check`. Run toolchain commands
  through mise after `mise install`; do not weaken release or policy gates.
- Shipping: require current PR CI including native amd64/arm64 Podman evidence,
  verify policy and squash merge, publish the matching immutable release tag within
  artifact retention, then read back release workflow success and GHCR index/
  platform identities. Production Sonarr/deployment testing is outside scope.

## Parallelization

Use one agent and one release PR. The cadence, action counters, and manual worker
share ownership boundaries; separate workers would add coordination overhead to
this small change.

## Local Verification Evidence

The focused logging suite passes, including the 29:59/30:00 boundary, manual
preview/execution/replay, two imports in one cycle, ambiguous responses, and
reservation/post-transmission save failures. Temporarily restoring INFO for
`issue decision` makes `TestDecisionLoggingLevels/json/INFO` fail on consecutive
minute decisions; the restored DEBUG implementation passes. The removal/search
test records actual JSON logs with immediate mutation/verification events and
an interval summary containing one new `submitted` outcome.

`mise run check` passes: formatting, vet, all race tests, 33 CI/release contract
tests, Renovate validation, actionlint, and no reachable vulnerabilities for
amd64/arm64. `mise run repo:inspect` reports no policy mismatches. Hosted native
image validation and publication remain release gates recorded on the PR and
GitHub Actions runs.
