# Sonarr Remediator Product Specification

Status: approved by the operator on 2026-10-12, alongside TECH.md. This document
defines the agreed behavior for the clean rewrite.
Episode-mismatch cleanup was amended by the operator on 2026-10-13.
Executable-warning cleanup was amended by the operator on 2026-10-17.

## Summary

A small service recovers completed downloads that Sonarr cannot import. It runs
primarily in rootless Podman, with rootless Docker as secondary compatibility,
uses only Sonarr's API, and can retry imports or safely import matched files.
Separately configured policies may remove broken
downloads, blocklist releases, and request replacements.

## Scope

The first release serves one Sonarr v4 instance per container. Radarr is a
future extension. Downloads that are still downloading, stalled in transit,
queued, or paused are outside the initial recovery scope.

Operators configure the service through YAML and environment variables,
inspect structured logs, and run local status/history commands. Deployment
requires one local state volume and no separate database service. The service
exposes no web server or network port and requires no media-directory mount.

## Behavior

### Boundaries and Configuration

1. Each running service connects to one configured Sonarr v4 instance. Sonarr
   is the only external application it contacts.

2. All download and import operations are requested through Sonarr. The
   service does not connect directly to download clients, indexers, metadata
   services, or any container engine's management interface.

3. The service neither reads nor modifies downloaded media directly. Previewed
   paths are Sonarr's paths and are submitted unchanged to Sonarr. Matching uses
   Sonarr's automatic preview, including unknown-series matches; the service
   does not guess a show or episode from a filename or override mapping.

4. Dry-run is the default and sends no mutating API requests. Active mode
   requires an explicit setting. Import recovery is enabled within active mode
   by default; cleanup, blocklisting, and replacement search each default off.
   Their effective settings and coupled effects are visible at startup.
   [Manual triggers](../manual-trigger/PRODUCT.md) add an explicit one-request
   execution exception and optional initial-wait bypass. Scheduled cycles keep
   their configured mode, waits and all other safety gates.

5. Enabled cleanup always asks Sonarr to remove the download from its download
   client, which can delete downloaded data. There is no queue-only cleanup
   mode. Blocklisting and replacement search remain separate permissions;
   enabling either without cleanup is rejected as invalid configuration.

6. Configuration precedence is built-in defaults, then YAML, then environment
   overrides. The Sonarr URL and credentials are required. Credentials can be
   an API key or a key read from a mounted secret file; conflicting credential
   sources, unknown keys, unreadable inputs, and invalid values fail startup.
   Credentials never appear in logs, query output, or evidence exports.

7. Optional series-ID and download-client-name exclusions apply to every action.
   Optional allowlists restrict action scope when populated; empty allowlists
   permit all otherwise eligible work. Exclusions take precedence. Unknown
   series are checked after preview resolves their identity; inability to
   establish required scope preserves the download.

### Observation and Waiting

8. The service starts observing immediately after local startup validation and
   polls every 60 seconds by default. Scans never overlap. Unreachable Sonarr
   delays observation without authorizing any action; authentication errors
   are clearly distinguished from connectivity errors.

9. Each successful queue scan covers every page and includes unknown-series
   items. Incomplete, inconsistent, oversized, malformed, or failed reads do
   not count as an empty queue and cannot authorize mutations or resolve an
   unseen issue as absent.

10. Initial candidates must be completed and tracked as `importBlocked`,
    `importFailed` or `importPending`. Completed import-pending downloads use
    the same observed-eligibility waits and policy checks as blocked downloads;
    they are not protected indefinitely by the pending label. Queued, paused,
    downloading, `importing` and `imported` work remains untouched. Warning/error
    text and a completed queue label alone do not override those protected states.

11. Recovery waits 15 minutes of observed eligibility by default. Cleanup waits
    six hours by default, except for Sonarr's explicit permanent episode/folder
    mismatch rejection or executable-file queue warning described in rule 27.
    These waits are configurable
    independently. A queue-added timestamp is diagnostic and never substitutes
    for observing that the download is completed in a candidate tracked state.
    A completed `importPending` download starts accumulating observed eligibility
    when first seen by this version; its prior queue age does not authorize an
    immediate automatic action. A valid Sonarr import rejection is also cleanup
    authority after `cleanup.wait` of observed rejected time, including
    indeterminate sample detection, mapping and access rejections.

12. Waiting progress survives restart. Unobserved downtime and failed-scan gaps
    do not advance the wait. A changed download identity or an observed return
    to a protected/noneligible state resets eligibility progress. Missing,
    future, or invalid timestamps never make a download immediately old.

13. Queue IDs identify rows; Sonarr download IDs identify shared downloads;
    series/episode IDs identify library targets. Release hashes embedded in
    titles or paths are not substituted for Sonarr download IDs. Multiple queue
    rows for one download produce one recovery or cleanup decision.

14. Issue status distinguishes waiting, recoverable, policy-blocked,
    retry-waiting, in-progress, uncertain, exhausted, and resolved work. A
    complete scan can resolve vanished work as externally resolved without
    claiming that this service performed the import.

### Import Policy

15. Every import attempt, including retries, refreshes the queue state, Sonarr
    preview, episode/file state, applicable profile, exclusions, permissions,
    cooldowns, and budgets. Read failure or a changed identity defers the
    action. Dry-run evaluates the same policy and reports the same decision.

16. The initial preview must identify a real series and positive episode IDs,
    with complete quality/revision information and no file rejection. A single
    file or a familiar title is insufficient without a valid match. Unknown
    enum values or missing policy inputs preserve the download.

17. Known queue episode identities must agree with the preview. A missing
    expected episode cannot be replaced by an older matched episode. When an
    expected episode has already been satisfied independently, the service
    must establish its current acceptable file before treating it as resolved.

18. Season packs and multi-episode files are supported when every affected
    episode has an unambiguous match and passes policy. Any rejected,
    unmatched, excluded, conflicting, or unresolved media file/episode defers
    the entire import decision. There is no partial policy override to force a
    pack through; ancillary files Sonarr does not return are not imported.

19. A missing episode file may be filled when all gates pass. Replacing an
    existing file additionally requires a strict improvement allowed by the
    series' current Sonarr profile, grouped quality ordering, custom-format
    settings, and proper/repack preference. Upgrade permission, cutoffs,
    minimum scores, and minimum score improvements are honored.

20. A clean preview with equal quality, equivalent revision, and equal score
    does not authorize replacement. Numeric quality IDs, resolution alone,
    global quality weight, or a custom-format score alone cannot establish
    profile preference. An unknown or contradictory comparison preserves both
    the existing file and the candidate.

21. Every preview rejection prevents import, including custom-format rejection,
    unresolved TheXEM mapping, and "Unable to determine if file is a sample".
    Sonarr's current rejection, whether temporary or permanent, authorizes
    cleanup after the rejected-time wait, except for immediate cleanup under
    rule 27. The service does not force an import.

22. A competing queued, downloading, paused, importing, or pending-import
    download for an affected episode defers intervention. Completed alternatives
    remain available until a verified outcome; the service does not rank and
    discard competing candidates as part of importing a winner.

23. Recovery makes at most three import request attempts per unchanged candidate
    by default, with at least 15 minutes between known failed attempts. A policy
    deferral consumes no attempt. A pending or uncertain attempt is not retried.
    Exhaustion is visible and prevents further imports; restart does not reset it.
    It does not independently authorize cleanup, but a valid Sonarr rejection
    can still qualify the download for rejected cleanup after its own wait.
    A materially changed candidate permits reevaluation without erasing history.

24. A command acknowledgement alone is pending. Verified success requires
    correlated import evidence for every intended file/episode and current
    acceptable episode-file state. Queue disappearance alone is insufficient.
    Partial success records the affected episodes and preserves remaining work.

### Cleanup and Replacement

25. Recovery normally takes precedence over cleanup; an executable-file warning
    instead selects immediate cleanup. Cleanup requires its own enabled setting,
    fresh complete observation, scope and safety checks. Age, retry exhaustion,
    a local/API error, or an empty preview never independently authorize deletion.
    A valid explicit Sonarr preview rejection is sufficient judgment for rejected
    cleanup; the executable-file queue warning needs no media preview.

26. Redundancy cleanup requires that every intended episode has an acceptable
    existing library file and every remaining previewed candidate is equal or
    inferior under the current profile. Missing intended episodes, uncertain
    pack membership, conflicting previews, and any unrelated rejection prevent
    redundancy classification; a valid rejection can instead qualify for
    rejected-download cleanup. Redundant releases are never blocklisted or searched
    again by this service.

27. Unusable-download cleanup is limited to complete, unambiguous
    previews where every remaining media file is positively identified by
    Sonarr as a sample or as having no audio tracks. Separately, rejected-download
    cleanup trusts any valid explicit Sonarr rejection after six hours by
    default, including indeterminate sample, disk/access, mapping and unknown
    rejection wording. One rejected file qualifies the entire shared download,
    including mixed packs; complete episode membership is not required. Empty,
    malformed, conflicting-download or unavailable previews remain protected.
    Configured client/series scope still applies; unknown series cannot establish
    membership in a restricted series scope. Generic rejected cleanup does not
    blocklist or search. It preserves existing library files and deletes the
    downloaded job/data through the download client.

    When Sonarr permanently rejects a file because its episode or episodes were
    unexpected considering the folder name, treat the shared download as failed
    and remove it from the queue and download client on the first eligible scan,
    without the cleanup wait. This applies to the exact singular/plural Sonarr
    verdict, including one mismatched file in a mixed pack; it does not infer a
    mismatch from queue warnings, filenames, or preview/queue IDs. Unrecognized
    wording and temporary rejections retain the configured rejected-time wait.
    Cleanup must be enabled; dry-run, scope, competing downloads, cooldowns,
    mutation limits, required reads and pending/uncertain-action protection still
    apply. Recheck the rejection before submission. Preserve library files and
    suppress blocklisting and replacement search. Reports identify the mismatch
    and show a zero cleanup wait without claiming a manual wait override.

    Sonarr's exact queue status message `Caution: Found executable file` authorizes
    immediate whole-download removal on the first eligible scan, including when
    the manual-import preview is empty. A filename ending in `.exe`, a status
    message title, or unrelated warning is not authority. Cleanup permission,
    dry-run, scope, protected tracked states, competition, cooldowns, mutation
    limits and pending/uncertain protections remain. Recheck the current warning
    before submission. Never execute downloaded files, blocklist, or search via
    this path. Preserve library files; report zero wait without a manual override.

    Rejected time accrues only between successful observations of rejection.
    It survives restart, excludes downtime/read-failure gaps, and resets when
    rejection clears, download identity changes, the download vanishes, or its
    tracked state becomes protected. A changed rejection message alone does not
    reset continuous blocking. Preexisting eligible age does not prefill this
    timer. Explicit manual wait override may skip it for one request.

28. Blocklisting may accompany verified unusable-download cleanup only when
    explicitly enabled. The actual removal request clearly identifies both
    client deletion and the blocklist choice. Automatic failed-download search
    is suppressed so replacement search follows the separately configured
    permission and verification policy.

29. Enabled replacement search runs only after verified unusable-download
    removal, for monitored episodes still missing an acceptable file, with no
    competing active download. Every affected episode is checked again. It is
    not requested for redundant releases, successful imports, or uncertain
    removals. Search submission is not a claim that a replacement was found.

30. Multi-step operations record each completed effect. A failed later step
    does not undo client deletion or blocklisting. Pending search work resumes
    only after fresh checks and available budget; restart never repeats a
    completed removal merely to continue its search.

### Limits, State, and Uncertainty

31. By default, at most one mutating HTTP request is attempted per scan and
    five per rolling hour. Attempts consume budget before transmission,
    including failures and ambiguous responses. Imports, deletion/blocklisting,
    and searches all share the budget. Read-only observation continues while
    budgets are exhausted; dry-run consumes no live mutation budget.

32. A verified completed action starts a 30-minute episode cooldown by default.
    Known failed recovery attempts follow their retry wait instead. A pending
    or uncertain action prevents competing actions for its download and
    episodes. Search that continues a verified removal is part of that same
    operation and may proceed without waiting for its own removal cooldown.
    An explicit manual `--override-wait` request may bypass completed-action
    cooldowns; pending and uncertain actions remain protected.

33. Observations, eligibility progress, attempts, unresolved outcomes, retry
    timing, cooldowns, and rolling budgets survive restart. A writable local
    state volume is required. State failure pauses new mutations immediately;
    there is no fallback to empty in-memory safety records.

34. Only one running service may own a state store. Operators deploy one active
    service per Sonarr instance; independent state volumes cannot coordinate
    duplicate instances. Local query commands may inspect the running service's
    state without starting another observer.

35. An ambiguous mutation response is recorded as uncertain and verified using
    reads. It is never blindly replayed. If available evidence cannot establish
    the result, the affected work remains blocked for operator attention. A
    restart or configuration toggle does not remove that protection.

36. Dry-run records recommendations separately from live attempts. Turning
    active mode on does not replay stored recommendations; the next scan
    evaluates current state and applies the normal limits.

### Operator Experience and Deployment

37. `status`, `issues`, `show <download-id>`, and `actions` are local read-only
    commands. They offer human-readable output and JSON, perform no Sonarr
    calls, and do not start background work. IDs in these commands are Sonarr
    download IDs or documented local issue identifiers, not release hashes.
    `triggers` also reads local live-request receipts. `trigger` requests one
    serialized cycle from the existing observer over private local control and
    defaults to a preview that does not persist state.

38. Logs and query output identify the download, affected episodes, action,
    reason, waits, attempted/verified outcome, and blockers. Repeated unchanged
    decisions are summarized; new failures and state transitions remain visible.
    Default log output is JSON on stderr with configurable verbosity.

39. Health distinguishes local process/storage readiness, Sonarr connectivity,
    and recent complete scans. The container uses a local health command and
    exposes no inbound endpoint. Sonarr outages mark degraded health without
    causing the service to reset state or exit in a restart loop.

40. Resolved issues and finished action records are retained for a
    default of 30 days. Unresolved attempts and state still needed for budgets,
    cooldowns, or retry protection are retained until safe to prune.

41. Rootless Podman is the primary build/runtime, with rootless Docker supported
    as secondary compatibility. A non-root application identity is required. The
    root filesystem and configuration are read-only; only a private local
    state volume is writable. The service needs no privileged mode, extra
    capabilities, container-engine socket, host networking, runtime ownership repair,
    or media mount. Unsupported state/config permissions fail before action.

42. SIGTERM/SIGINT stops new scans and mutations, preserves pending outcomes,
    and closes state within a 25-second application deadline. Container engines
    allow 30 seconds for shutdown. Stopping or switching to dry-run prevents
    new actions; it cannot undo commands already accepted by Sonarr.

43. Release targets are Linux amd64 and arm64, with immutable version
    images, bundled HTTPS trust roots, a primary rootless Podman recipe, and
    a secondary rootless Docker Compose recipe. Podman validation is the required
    CI gate; Docker has no equal PR gate. Production deployment automation is
    future work. [The Actions spec](../github-actions/PRODUCT.md) defines the
    approved repository/release policy for the recreated repository.

## Final Decisions

- Replacement requires strict improvement. Equal accepted files can qualify
  for separately enabled redundancy cleanup.
- Unusable cleanup supports only Sonarr's explicit `Sample` and
  `No audio tracks detected` verdicts, subject to invariant 27's full checks.
- JSON logs, 30-day resolved-history retention, a 25-second shutdown deadline,
  and Linux amd64/arm64 images are approved defaults.
- The operator updated the container priority on 2026-10-12: rootless Podman
  is primary for builds and runtime; rootless Docker is secondary compatibility.

## Historical Context

The recovered specifications inform discovery; they do not automatically
authorize features or defaults for the rewrite. [SOURCES.md](SOURCES.md)
records the recovered revisions, current API evidence, and browser-flow evidence.
