# Manual Trigger Product Specification

Status: implemented for target release 0.2.0. Publication remains a
separate operator action.

Patch 0.2.1 extends the explicit wait override to episode cooldowns without
requiring deployment configuration changes or a state migration.

Release 0.3.0 includes completed import-pending downloads and generic
Sonarr-rejected cleanup under the updated base contract. Existing manual waits
and their explicit override apply; no new deployment configuration is required.

## Summary

Operators can preview or request one immediate recovery/cleanup cycle without
changing the running service's configuration. An explicit wait override skips
initial recovery and cleanup delays and episode cooldowns for that request while
retaining policy, permissions, budgets, retry protection and correlated outcome
verification.

This extends [the base product contract](../sonarr-remediator/PRODUCT.md).
The manual request is an explicit exception to its background dry-run and initial
wait and episode cooldown rules; the remaining safety and deployment boundaries
continue to apply.

## Behavior

### Commands and Scope

1. `trigger` requests a bounded evaluation from the running observer. It does
   not start a second observer. An unavailable observer produces an explicit
   error; the command does not fall back to independent execution.

2. `trigger` defaults to preview. `trigger --dry-run` makes that choice explicit.
   Neither invocation sends a mutating Sonarr request, changes persistent state,
   advances observed waiting progress, consumes a live budget, or changes the
   background service's mode. Read-only Sonarr calls are permitted.
   Preview does not pause an active background observer; its normal work can
   occur before or after the preview cycle.

3. `trigger --execute` explicitly permits live work for this one request, even
   when the background observer is configured with `dry_run: true`. It does not
   turn active background mode on. Subsequent scheduled cycles retain their
   configured mode and permissions.

4. `--dry-run` and `--execute` are mutually exclusive. `--dry-run=false` cannot
   authorize execution; use `--execute`. Invalid or unknown options fail before
   observation or mutation. Commands do not prompt interactively.

5. Without a selector, a trigger evaluates all current queued downloads and
   relevant pending replacement-search continuations within configured scope.
   Repeated `--download-id ID` flags restrict work to those exact Sonarr download
   IDs. Duplicate IDs select one shared download once. Queue-row IDs, titles,
   paths and guessed release hashes are not alternative identifiers.
   A request selects at most 1,000 distinct downloads. Larger selections fail
   before mutation and require narrower IDs; results are never silently truncated.

6. Selected IDs absent from both the current queue and relevant pending
   continuations receive a `not_present` result. Historical issue records alone
   never authorize new work. An empty queue is a completed no-action result only
   after a successful complete queue read.

7. Selection restricts new mutations, not the evidence used for safety checks.
   The service still checks the complete queue for competing work. It cannot
   hide an excluded, pending or unidentified competitor by selecting one ID.

### Wait Override and Permissions

8. Without `--override-wait`, the configured observed-eligibility waits and episode
   cooldowns apply. The base policy's permanent episode/folder mismatch cleanup
   has zero initial wait, including on the first observation, and reports no
   manual bypass. `--override-wait` skips `recovery.wait`, `cleanup.wait` and
   `limits.episode_cooldown` for this manual request, including cooldowns from
   another download affecting the same episode. It does not lower the stored
   waits or fabricate elapsed observation.

9. `trigger --dry-run --override-wait` previews what an equivalent one-off live
   request could submit now. `trigger --execute --override-wait` may submit that
   work after fresh checks. The override ends when this request finishes.

10. A download on its first eligible observation can be considered with an
    explicit wait override. It must still be completed and tracked as
    `importBlocked`, `importFailed` or `importPending`. Downloading, queued,
    paused, `importing` and `imported` downloads remain protected. An explicit
    wait override can consider completed import-pending work on its first
    observation, but cannot bypass preview rejections or other policy gates.

11. Execution never enables a disabled recovery, cleanup, blocklist or
    replacement-search permission. Scope, competing-download protections and
    action locks remain in force. Import and strict cleanup retain matching,
    complete episode membership and profile rules; generic Sonarr-rejected
    cleanup uses Sonarr's rejection instead of those independent proofs.
    There is no force-import or unrestricted force-delete flag.

12. Wait override does not bypass known-failure retry spacing,
    recovery attempt exhaustion, mutation budgets, state/read failures, or
    pending, partial and uncertain outcomes. Blocked work is explained and kept.

13. Recovery continues to take precedence over cleanup. Cleanup accepts proven
    redundancy, complete explicit Sample/No audio tracks verdicts, or a valid
    explicit Sonarr import rejection under the base contract. Rejected-download
    cleanup uses observed rejected time for its wait, except for permanent
    episode/folder mismatches with zero wait; a manual wait override can skip
    the configured wait. Mapping, membership, sample-detection and access
    rejections can qualify. Empty/unavailable/malformed previews and local/API
    failures cannot.

14. Cleanup output explicitly states that removal uses the download client and
    can delete downloaded data. Blocklisting and replacement search choices are
    shown separately. Redundant releases are never blocklisted or searched again.

15. Replacement-search continuations can be considered for selected downloads
    even after their queue rows disappear, but only after verified authorized
    removal and fresh permission, missing-episode, competition and budget checks.
    A trigger does not complete a multi-step operation through unbounded scans.

### Planning, Limits and Results

16. A trigger performs one bounded cycle, not a background run or queue-draining
    loop. Per-scan and rolling-hour budgets are shared with automatic cycles.
    With current defaults, one trigger can attempt at most one mutation. Imports,
    deletion/blocklisting and search all consume that same allowance.

17. A preview distinguishes an eligible policy action from an action selected for
    submission within this cycle's available budgets. Later eligible items are
    reported as limited, not as additional actions this invocation will perform.
    Applying limits virtually does not reserve or consume actual budget.

18. Planning order is deterministic: already-due work first in its existing due
    order; override-only work next by first observed eligibility and then exact
    download ID. Preview and live execution use the same ordering and gates.

19. A preview is current evidence, not a stored authorization or replayable plan.
    `--execute` always rereads required evidence and reevaluates policy. Changed
    identity, preview, permissions, library state or budgets can change its result.

20. Human-readable output and `--json` identify mode, selection, effective
    permissions, override choice, queue rows versus shared downloads, available
    budgets, and per-download action and blocker reasons. Each item includes
    affected episodes, preview-file count, configured wait, observed progress,
    remaining wait and whether that wait was bypassed.
    It distinguishes unavailable/empty preview from incomplete intended membership
    rather than presenting both as an age delay.

21. Results distinguish `would_import`, `would_remove_redundant`,
    `would_remove_unusable`, `would_remove_blocked`, `would_search`, `waiting`, `blocked`, `limited` and
    `not_present`. Execution additionally reports attempted actions and their
    acknowledged, pending, verified, failed, partial or uncertain outcomes.
    Machine output has a versioned schema; runtime logs use stderr.

22. Successful submission is reported as pending until correlated verification
    succeeds. Neither command completion nor queue disappearance alone proves
    import success. A manual removal uses the existing acknowledgement and
    complete queue-absence contract; blocklist effects need their own evidence.

23. The command returns after its bounded submission cycle. Later ordinary
    observer cycles verify outstanding live attempts, including when background
    mode is dry-run. They cannot replay the manual request or inherit its wait
    override. Disabled background mutations remain disabled; a search continuation
    may need another explicit trigger or already-enabled active background mode.

### Ownership, Cancellation and History

24. Manual and automatic cycles never overlap. At most one manual request may
    be executing or waiting for the current automatic cycle. Concurrent requests
    receive a `busy` result. Repeated requests cannot indefinitely starve scheduled
    observation, and no persistent backlog of executable commands is created.

25. `--timeout` bounds queueing, reads and submission; its default and maximum
    are two minutes. Expired/cancelled requests cannot start new mutations.
    SIGINT stops new manual work without disabling background observation.
    Already accepted Sonarr commands may still complete.

26. Live requests receive an identifier and durable receipt before a mutation
    can be sent. `triggers` and `triggers --request-id ID` inspect live request
    history locally in human or JSON form, without contacting Sonarr or starting
    work. Preview does not create a persisted live request or recommendation.

27. A lost response or process interruption cannot cause automatic resubmission.
    The CLI does not retry a sent execution request. The receipt and existing
    action records expose known effects and unresolved outcomes; a request that
    never reached durable acceptance may have no receipt. Operators can list
    recent receipts when an acknowledgement was lost.

28. Repeated delivery of an accepted live request identifier cannot execute it
    twice. A different payload with the same identifier is rejected. Expired
    unaccepted messages are rejected, and restart never resumes unsubmitted work.
    Starting a new trigger is a new operator decision and still faces all gates.

29. Receipts identify manual source, selected scope, effective execution mode,
    wait override, coupled effects and related live attempts. Finished receipts
    follow normal history retention; unresolved effects remain retained. No
    private credentials, raw environments or credential-bearing URLs are output.
    Reused examples, fixtures and shared reports contain synthetic or anonymised
    runtime data.

30. Exit `0` means the evaluation completed with at least one planned action or
    an acknowledged live submission; it does not assert verified external success.
    Exit `2` means a completed evaluation with no action selectable now, including
    empty, waiting, blocked, limited or absent work. Exit `1` covers invalid input,
    unavailable/busy service, failed required reads and local failures. Exit `3`
    means execution may have reached the service or Sonarr but its result is
    uncertain; output explains how to inspect it. SIGINT returns `130` with the
    same warning when transmission may have occurred. JSON reports these reasons.

### Deployment and Compatibility

31. Existing rootless Podman/Docker deployments require only an image-version
    update. No new mount, host port, engine socket, capability, environment setting
    or startup command is required. The manual control endpoint is private and
    local; it is not an HTTP service or a remotely accessible API.

32. Local control is available to the application's existing private-state owner.
    Other identities cannot submit requests. CLI requests use the running
    observer's effective configuration; callers cannot supply credentials,
    alternate Sonarr endpoints, profile overrides or new policy permissions.

33. If local control cannot be established safely, existing automatic observation
    continues and status explains that manual control is unavailable. Manual
    commands fail clearly instead of creating an insecure fallback or demanding
    a deployment change.

34. The update preserves configured background mode, permission settings, waiting
    progress, budgets and unresolved effects. Any required store upgrade is
    automatic and transactional. Older images may not support the upgraded
    store; rollback must preserve effects and cannot discard safety history.
    Any required deployment change beyond image version makes the implementation
    a breaking deployment change under repository guidance and requires spec review.

## Operator Flow

These commands are available in the 0.2.0 implementation.

```sh
podman exec sonarr-remediator /sonarr-remediator trigger --dry-run --override-wait
podman exec sonarr-remediator /sonarr-remediator trigger --execute --override-wait
podman exec sonarr-remediator /sonarr-remediator trigger --dry-run --override-wait --download-id download-example-001 --json
podman exec sonarr-remediator /sonarr-remediator triggers --request-id request-example-001 --json
```

Use equivalent `docker exec` commands for rootless Docker. The example identifiers
are synthetic. Executing a previewed command still performs fresh evaluation.
