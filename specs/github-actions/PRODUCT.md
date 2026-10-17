# GitHub Actions and Repository Rules Product Specification

Status: approved by the operator on 2026-10-12, alongside TECH.md and the
Podman-primary amendments to the service specifications.

## Summary

The fresh public repository admits changes to `main` only through pull requests
that pass one CI workflow for their current revision. Native rootless Podman
builds and runtime checks are required on amd64 and arm64. Explicit releases
publish those exact tested images to public GHCR without rebuilding or
rerunning CI.

## Scope

This specification configures the recreated repository for the rebuild only.
It assumes no inherited workflows, protection rules, release automation, or
CI artifacts from the old repository. The companion [TECH.md](TECH.md) defines
workflow structure, repository settings, and setup/verification steps.

Rootless Podman is the primary build and runtime. Rootless Docker is secondary
compatibility and has no equally required PR gate. Application behavior remains
defined by [the service spec](../sonarr-remediator/PRODUCT.md).

## Behavior

### One CI Workflow Before Merge

1. Opening a PR targeting `main` starts one CI workflow for that PR revision.
   A new source commit starts a new run. Ordinary branch pushes without a PR,
   pushes/merges to `main`, and PR metadata-only changes do not start another
   CI run. Draft PRs use the same checks; marking one ready does not duplicate
   a previously completed run.

2. A newer revision cancels superseded CI for the same PR. A cancelled,
   superseded, failed, missing, or unexpectedly skipped prerequisite cannot
   make the current revision mergeable. No result is reused from a different
   source revision, base content, or recreated repository.

3. Every PR follows the same required gates, including documentation, workflow,
   version, and dependency changes. Path filters do not hide the required check.
   Missing build/test inputs produce a failure rather than a successful skip.

4. CI checks Go formatting, vet, tests with the race detector, and Go dependency
   vulnerabilities. It builds and smoke-tests production images using native
   rootless Podman on Linux amd64 and arm64. All these checks must succeed.

5. Rootless means the Podman engine/build runs as a non-root user and reports
   rootless operation; the application also runs as a non-root user in its
   container. A rootful build, privileged nested container, or silent fallback
   to Docker cannot satisfy the Podman check.

6. Runtime checks cover configuration/secret readability, narrowly writable
   state, read-only root, dry-run with no mutations, local health/query commands,
   persistence across container recreation, and bounded shutdown. They use
   controlled fake Sonarr data; CI has no live Sonarr credentials, SSH access,
   or dependency on the production host.

7. The PR exposes one stable required check named `CI`. It succeeds only when
   every named prerequisite succeeds and the expected image/evidence artifacts
   are available. This check is attributable to GitHub Actions, rather than an
   arbitrary status publisher. Job details remain available for diagnosis.

8. CI runs with read-only repository access, no publishing permissions, and no
   production secrets. Fork contributions can run after GitHub's normal
   approval to execute an external contributor's workflow, where required;
   pending approval blocks merging. That workflow-run approval is separate
   from an independent PR-review requirement.

9. Normal success uses one workflow run per revision. A maintainer may explicitly
   retry a failed/interrupted run or recover expired evidence; retry does not
   authorize merging without a successful current result. No automatic main
   CI or release-time rebuild is used to compensate for missing PR evidence.

### Main Protection and Repository Rules

10. `main` is the default protected branch. All post-bootstrap changes enter
    through PRs, including owner changes and workflow edits. Direct pushes,
    force pushes, deletion, and routine administrative bypass are disallowed.

11. Merging requires successful `CI`, the source branch being up to date with
    `main`, and resolved review conversations. If `main` moves, the author
    updates the branch and the resulting revision must pass CI again. Multiple
    ready PRs do not justify merging a stale result.

12. Independent reviewer approval is not mandatory. The maintainer still
    decides when to merge and reviews changes to workflows, permissions,
    publishing, and repository rules. Optional reviews are supported without
    introducing a second-person requirement for a solo maintainer.

13. Only squash merging is enabled, producing a linear `main` history. Merge
    commits and rebase merging are disabled. Successfully merged source
    branches are deleted automatically. Auto-merge, if later enabled, must
    obey the same gate; initial setup uses explicit maintainer merges.

14. Release tags matching `v*` cannot be moved or deleted after creation, with
    no routine owner bypass. Creating an explicit release tag is permitted;
    publishing additionally verifies that it points to the corresponding
    successfully checked squash merge on `main`.

15. Actions are enabled with read-only token permissions by default. Third-party
    actions require an intentional allowlist decision, and all referenced
    actions are pinned to full commit SHAs. The chosen toolchain, vulnerability
    checker, and build inputs have recorded/pinned identities.

16. Repository creation may seed an initial root commit containing the approved
    rebuild specifications and setup metadata before branch rules exist. This
    creation step is not a merged implementation. After bootstrap, the required
    rules apply to every implementation/workflow PR. Bootstrap does not provide
    a standing bypass or copy old implementation/history into the fresh repo.

### Tested Images and Five-Day Retention

17. A checked-in `VERSION` file determines the image/application version during
    PR CI. It uses semantic version form without a leading `v`, for example
    `1.0.0` or `1.1.0-rc.1`. An explicit release tag must be exactly `v` followed
    by that value. Version changes are normal PR revisions requiring CI.

18. CI retains the tested OCI image for each architecture and evidence linking
    both to the repository identity, PR, source/base/checkout identities,
    tested source tree, image identity, version, and successful required gates.
    CI does not upload raw credentials or production media data.

19. Image/evidence artifacts expire after five days. Operators publish within
    that window. Missing, expired, corrupt, mismatched, or superseded artifacts
    block promotion; no untested substitute or implicit rebuild is allowed.
    Recovery requires an explicit maintainer decision to retry the same CI
    revision where possible or prepare a new checked release revision.

20. Stored artifacts are the exact images that passed native runtime checks.
    Publishing may assemble a multi-platform index but cannot rebuild layers,
    change embedded version/source metadata, or replace either architecture
    with an image from another revision/run.

### Explicit Release Publishing

21. Publishing starts only when a maintainer explicitly publishes a GitHub
    release for an allowed `v` version tag. Draft releases do not publish.
    An ordinary merge, tag push alone, or PR CI completion does not publish.

22. Before publishing, the release verifies the tag/version, merged PR and
    successful run in the fresh repository, exact tested source-tree agreement
    with the tagged squash commit, both image artifacts, and complete evidence.
    A directly pushed or unverified commit cannot satisfy this check.

23. Version tags such as `v1.0.0` are immutable in GHCR. An existing tag with the
    same expected image/index digest is an idempotent result; a different digest
    is a collision and fails without overwrite. Release-native architecture
    components receive the same immutability treatment.

24. Stable releases publish a manifest/index containing native amd64 and arm64
    images. `latest` advances only after the immutable version is complete and
    verified. A prerelease never moves `latest`; an older stable release never
    downgrades it. No major/minor mutable aliases are introduced initially.

25. Release publishing is serialized and does not cancel an in-progress release
    for a newer one. Failure may leave staged architecture images, but cannot
    move `latest` to an incomplete release. Partial results are visible; an
    explicit retry resumes idempotently when its required evidence remains.

26. Registry authentication uses the scoped workflow token; only release jobs
    needing to push have package-write permission. No long-lived PAT, live
    Sonarr key, production SSH key, or privileged engine is required.

27. The repository and published image package are public. Initial setup verifies
    actual GHCR package visibility and publishing access; repository visibility
    alone does not establish package visibility. Recreating a repository does
    not silently grant access to an existing package or permit old-version
    collisions. Anonymous users can pull a completed public release.

28. The release reports the immutable image digest, available platforms, tested
    PR/run, and source/version relationship. The source is the image's recorded
    tested revision; the squash commit may have a different commit ID while
    retaining the exact tested tree. Production deployment is a later operator
    action, not an effect of CI or release publishing.

## Final Decisions

- One PR CI workflow per revision; no duplicate CI after merging to main.
- Native rootless Podman validation on amd64 and arm64; Docker secondary.
- Owner-enforced PR/check/up-to-date rules, squash merges, and zero mandatory
  independent approvals.
- Exact tested image promotion from five-day artifacts, with VERSION set
  before merge and explicit release publication to public GHCR.
- Immutable version/component tags and stable `latest`, with no downgrade or
  prerelease promotion of `latest`.
- Renovate maintains only the five `mise.toml` tool pins. Dependabot maintains
  Go modules, container image inputs, Compose images and full Action SHAs through
  ordinary CI-gated PRs. Automerge is disabled; separate-bot Go updates require
  synchronized pins before merge. There is no scheduled duplicate CI workflow.
- CI and release runners remain on Ubuntu 24.04 for amd64 and arm64. Runner labels
  and explicit workflow mise runtime versions are maintained manually; platform
  migration must validate workflow lint and native Podman on both architectures.
