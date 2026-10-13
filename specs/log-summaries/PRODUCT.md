# Log Summaries Product Specification

Status: approved and implemented for release 0.4.1.

## Summary

Reduce routine INFO logging to compact summaries so operators can see service
activity without a decision for every waiting download on every poll. Preserve
immediate action visibility and opt-in detailed decisions at DEBUG.

## Behavior

1. At the default INFO level, routine per-download `issue decision` records are
   suppressed, including new detections, changing wait progress, policy blockers,
   dry-run recommendations, and external resolution. Polling and decisions still
   run at the configured polling interval.

2. `logging.level: debug` enables the existing detailed `issue decision` records,
   with download ID, episode IDs, state, and reason. Existing deduplication of
   identical state/reason pairs remains; changing wait progress remains visible.
   The existing logging environment override also works. No new `log_level` key
   or deployment setting is required.

3. One INFO `service summary` appears after the first automatic observation cycle
   on each service start, including a deferred observation. The summary describes
   the current service state; it does not claim that an unsuccessful scan supplied
   fresh issue information.

4. Subsequent summaries appear on the first automatic cycle completing at least
   30 minutes after the last summary. Ordinary issue changes and dry-run
   recommendations do not trigger extra summaries. The interval is fixed at
   30 minutes for this release and is checked at cycle boundaries; it does not
   require another timer or change polling frequency.

5. A cycle that actually attempts one or more Sonarr mutations emits one extra
   summary at its end, without waiting for the interval. This includes imports,
   queue removals, and replacement searches, whether automatic or manually
   executed, and includes ambiguous responses. A saved intent without a mutation
   call, blocked plan, or preview is not an executed action.

6. A summary resets the 30-minute interval. When startup, interval, and action
   triggers coincide, emit one summary. Multiple actions within one cycle produce
   one summary; retain their individual immediate action records.

7. Each summary contains counts, without per-download lists or repeated decision
   reasons: current active issues by state (excluding resolved history), unresolved
   action count, real mutation attempts by kind since the previous summary, newly
   observed action outcomes by outcome since the previous summary, and new dry-run
   recommendations since the previous summary. An outcome is counted when an
   actual attempt first receives it or changes to it, not on every poll while it
   remains pending. The summary also reports dry-run mode, Sonarr connectivity,
   last complete scan time, and rolling-hour mutation budget usage and limit.

8. Empty queues still receive startup and periodic summaries with zero active
   issues. A deferred observation can receive a due summary with degraded
   connectivity and the last complete scan time. Retained resolved history must
   not inflate active issue counts. Historical attempts loaded at startup must
   not be reported as newly executed actions or new outcomes.

9. Existing mutation and successful verification INFO records remain immediate.
   Replacement-search submissions also receive an immediate mutation record.
   Warnings and errors remain visible immediately at their existing levels;
   this change does not throttle outages or verification warnings. Later outcome
   changes without a new mutation are included in the next periodic/action
   summary rather than causing another summary themselves.

10. Read-only manual previews do not emit summaries, consume/reset the daemon's
    summary interval, or affect its summary counters. Manual execution shares
    the daemon's summary interval and counters. Replaying a completed manual
    receipt does not report a new action.

11. JSON and text logging both support this behavior, using the existing stderr
    destination. WARN and ERROR thresholds suppress INFO summaries and DEBUG
    decisions as usual. Summary intervals/counters restart with the process;
    they are not persistent history or an audit substitute for `actions`.

12. Release 0.4.1 requires only an image-version update for existing Docker/Podman
    deployments. Configuration, state schema, permissions, health behavior,
    policy, waits, budgets, and mutation safeguards remain compatible. Release
    notes identify the change in INFO output and explain how to enable DEBUG.

## Delivery

After explicit approval of this spec, implement and verify the change, create a
PR, merge after required checks and repository policy pass, and publish v0.4.1
through the existing GitHub/GHCR release workflow. Verify publication of both
architectures and the versioned image index. Updating a live deployment is
outside this release task.
