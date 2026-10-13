# GitHub Actions and Repository Rules Technical Specification

Status: approved by the operator on 2026-10-12, alongside PRODUCT.md. This is
the workflow and repository-configuration implementation plan.

## Context

[PRODUCT.md](PRODUCT.md) defines the fresh repository's merge and release
contract. [The service technical spec](../sonarr-remediator/TECH.md) defines Go,
one shared Containerfile, native amd64/arm64 images, and rootless runtime checks.
Podman is now primary; Docker remains secondary compatibility.

The target repository and GHCR package are public. Do not migrate the old
repository's CI, Release Please, workflow triggers, protection assumptions, or
proof artifacts. Repository IDs and workflow identities are part of new evidence
so a recreated repository cannot silently inherit trust from the old one.

The specification was written before application, Containerfile, or workflow
implementation. The service scaffold is now present, and this implementation
extends its initial CI/evidence scripts. Official GitHub documentation confirms native
`ubuntu-24.04-arm` hosted runners, ruleset status-check requirements, and artifact
retrieval. The GitHub Actions app ID was read as `15368`; verify it during setup
before binding the required check to that integration.

## Proposed Changes

### Two Workflows

Use `.github/workflows/ci.yml` and `.github/workflows/publish.yml`. CI owns all
merge checks; publishing only validates/promotes successful CI artifacts.

CI triggers on `pull_request` targeting `main`, types `opened` and `synchronize`.
Drafts run normally. No `push`, `pull_request_target`, metadata-edit, ready-for-
review, release, or scheduled duplicate test trigger is added. Reopening an
unchanged PR retains its previous checks; a missing result needs a new revision
or explicit workflow retry. A manual retry is an exception, not a third workflow.

Use per-PR concurrency with `cancel-in-progress: true`. Set `contents: read` as
the workflow token permission. No job in PR CI gets package-write, contents-write,
production secrets, or production SSH access. All referenced actions use full
reviewed commit SHAs with human-readable version comments.

The checkout tests GitHub's PR merge ref and records its tree and commit alongside
the event's source-head and base SHAs. Use explicit PR identity from the event;
never infer a download/build/source identity from a PR title or branch-name shell
string. Repository protection requires updating against current main before merge.

| Job | Runner | Responsibility |
| --- | --- | --- |
| `go` | `ubuntu-24.04` | Formatting, vet, race-enabled tests, source vulnerability checks for both Linux targets |
| `podman` matrix | `ubuntu-24.04` / `ubuntu-24.04-arm` | Native rootless build, runtime smoke checks, OCI export and per-platform metadata |
| `gate`, display name `CI` | `ubuntu-24.04` | Always evaluate prerequisite results, validate both artifacts, emit successful promotion receipt |

Run the Go job once. Its script checks `gofmt` without rewriting files, then
`go vet ./...`, `go test -race ./...`, and pinned `govulncheck` source checks for
Linux amd64/arm64. Race tests use the host's compiler support; production builds
remain static with `CGO_ENABLED=0`. A confirmed reachable vulnerability fails
the check. A vulnerability-database/tool read failure is failure, not a clean
bill of health. No separate PR-title, CodeQL, or image-vulnerability workflow
is introduced in the initial plan.

Use the same fully specified Go patch version from `go.mod` in setup/build
inputs, with `GOTOOLCHAIN=local`. Pin the vulnerability-checker version; never
install `@latest`. Base images use reviewed digests, and the Go builder/runtime
identities agree with the intended platform. Dependency/cache keys include
toolchain, target, and module-lock identities; restored caches never bypass
checks or substitute a previously built application for changed source.

Implementation uses SHA-pinned `jdx/mise-action` with an explicit mise version,
and `mise run check`/`mise exec --` for project tooling in all jobs. It is the
sole intentionally allowlisted third-party action. Renovate's native `mise`
manager updates runtimes/checkers; grouped Go updates include go.mod and both
Go Containerfiles. Contract tests fail when their patch versions differ.
Renovate also updates full action SHAs, workflow mise versions, and image digests.
A package rule disables only `ubuntu` from the `github-actions` manager's
`github-runners` datasource, including its arm64 variant. Runner labels stay on
`ubuntu-24.04` and `ubuntu-24.04-arm` until a manual migration validates
actionlint and native rootless Podman on both architectures.
All `*Containerfile` names are explicitly matched, including the fake fixture
and official Renovate validator image. Its validator runs rootlessly from a
digest-pinned upstream image because installing its npm graph triggered mise's
provenance-downgrade protection. No trust-policy exemption is configured.

Give Go checks a 15-minute timeout, each native container job 25 minutes, and
the gate five minutes. Matrix `fail-fast: false` preserves both platform failure
reports. Failed jobs upload sanitized diagnostics where useful, not a promotion
receipt. Timeouts, runner shortage, and cancellation remain blocking failures.

### Native Rootless Podman

Start each native job by asserting its runner architecture, `id -u != 0`,
`podman info` rootless status, and usable user-namespace/storage setup. Record
Podman version, architecture, UID, rootless flag, and storage driver. Installing
missing host tooling on a disposable runner may use runner administration;
all Podman build/run/load/push operations themselves remain unprivileged.
No `sudo podman`, Docker fallback, privileged container, or host engine socket
counts as rootless acceptance. An unsupported runner setup fails visibly.

Build the declared production inputs with `podman build -f Containerfile`, native
architecture, OCI format, fixed source/version metadata, and the same non-root
application identity as deployment. Rootless Docker's documented equivalent is
`docker build -f Containerfile`; it is not called from the required workflow.
Use explicit COPY/build-context exclusions so keys, local state, raw HARs,
media, test artifacts, and Git credentials cannot enter the production image.

Production CI builds use `--no-cache`: local Podman 5.4.2 reused stale LABEL values
after revision build-argument changes. Checkout uses depth two to validate both
merge-ref parents against the event; workflow IDs come from the current run API,
which also works before the new workflow exists on the default branch.

The smoke harness creates a private Podman network and controlled fake Sonarr
container. Run the production image with a read-only root, all capabilities
dropped, no privilege escalation, read-only config/secret inputs, and a private
local state volume. Check local health/status/query output, repeated observations,
dry-run's zero mutation count, persistence after recreation, and shutdown within
the documented 25/30-second budgets. No host-network or live Sonarr shortcut.

The primary run recipe uses `--userns=keep-id:uid=65532,gid=65532` so the invoking
account maps to the application's container UID/GID. Verify read-only config
and private state access without host ownership repair. Docker's secondary
recipe verifies its own namespace/volume mapping instead of assuming this
Podman option transfers to Docker.

Only after runtime success, export that exact image as an OCI archive. Record
its image-config and OCI manifest identities, archive SHA-256, platform, version,
source revision, tested tree, and runtime results. Export/import must preserve
the OCI manifest/config/layer identities; if the toolchain cannot preserve them,
fix the packaging before claiming exact-image promotion. Publisher metadata
must not be restamped with the later squash commit or release event time.

### Gate and Artifact Receipt

The `gate` job uses `if: always()` and explicitly demands `needs.go.result ==
success` and `needs.podman.result == success`. It downloads fixed expected
artifacts for both platforms and validates their metadata and hashes. Missing,
cancelled, skipped, mismatched, or failed prerequisites cause the gate to fail.
Do not make an empty success-only shell step stand in for this check: GitHub
can otherwise treat skipped jobs as acceptable status checks.

Use separate named artifacts per platform and run attempt, such as
`image-amd64-<run>-<attempt>` and `image-arm64-<run>-<attempt>`. Avoid matrix-job
outputs that overwrite the same digest field. A complete workflow retry produces
a fresh matching set; partial retries cannot silently mix attempts. Promotion
uses only a receipt produced after the complete required set succeeds.

The receipt is structured JSON, containing repository ID/name, workflow path/ID,
run ID/attempt, PR number, source-head/base/checkout commit SHAs, tested Git tree,
VERSION, prerequisite outcomes, architecture artifact IDs/names/hashes, image
manifest/config digests, rootless runtime evidence, and creation/expiry context.
Cross-check platform claims against each archive, not just a matrix label.

Set `retention-days: 5` on both image archives and the receipt. Large images
are not retained as separate duplicate build and release artifacts. Artifact
expiry does not automatically revoke a formerly green merge status; it does
prevent publication. Recovery never invents a receipt or rebuilds on main. An
explicit full retry of the associated PR run is allowed if its original source
is still resolvable and it produces matching evidence; otherwise use a new
release-preparation PR and tag. A new tag must respect existing immutable tags.

### Repository Settings and Rulesets

Set the fresh repository public, default branch `main`, Actions enabled, squash
merge enabled, merge-commit/rebase merge disabled, automatic branch deletion
enabled, and auto-merge disabled initially. Retain issues. Rulesets, rather than
overlapping classic branch-protection settings, are the authoritative policy.

Create one active branch ruleset matching `refs/heads/main`, with no bypass actors:

| Rule | Setting |
| --- | --- |
| Pull request required | Zero required independent approvals; no CODEOWNER or last-push approval requirement |
| Review conversations | Resolution required |
| Required status | Literal emitted check-run name `CI`, bound to GitHub Actions integration |
| Current base | Strict up-to-date status policy enabled |
| History | Linear history required |
| Force pushes / deletion | `non_fast_forward` and `deletion` prohibited |

Do not add a generic update restriction that accidentally blocks all legitimate
PR merges. Required PRs already block routine direct updates; an empty bypass
list keeps the owner subject to the same gate. Do not require independent review
implicitly through another ruleset or organization policy without reporting it.

Create an active tag ruleset matching `refs/tags/v*`, prohibiting all updates
(`update`) and deletion with no bypass actors. Do not prohibit initial creation
of release tags. Repository write permissions control who may create/publish a
release; the publisher still independently checks source and CI evidence.

Set default workflow-token permission to read-only and disallow Actions-created
PR approvals. Enable full-SHA action pinning enforcement where the fresh repo's
API supports it; repository/workflow validation also rejects unpinned references.
Allow the explicitly used GitHub-owned actions; do not enable an unrestricted
third-party catalog. Leave publishing writes as explicit job permissions.

### Explicit Release Promotion

The publishing workflow is present on `main` and triggers on
`release: types: [published]`. Accept tags of form `vMAJOR.MINOR.PATCH` or a
semantic prerelease; reject build-metadata/tag forms unsupported by OCI naming.
Read VERSION without a leading `v` or build metadata. Require the GitHub release's
prerelease flag to agree with the version form. No tag-push or main-push publisher.

Use one serialized repository/package concurrency group, without cancelling
in-progress releases. Before any write, the verification job checks:

1. The release tag resolves to a commit reachable from the fresh repository's
   main and to the expected merged PR's `merge_commit_sha`.
2. The associated PR targeted main, was squash-merged, and has the recorded head
   revision. Its run used the expected `ci.yml`, event, repository ID, and
   successful complete attempt/gate; a similarly named unrelated check is not proof.
3. The release commit's Git tree exactly equals the tested checkout tree in
   that run's receipt. Commit IDs need not match after squash merging.
4. Tagged VERSION, receipt version, image labels/application version, and release
   version agree; both expected archives remain available within five days.
5. Archive hashes, platform/config/manifest identities, and native rootless
   runtime evidence match. A stale artifact from another run/attempt is rejected.

Use GitHub's merged-PR/commit association API to find the candidate run, then
cross-check the receipt. Do not rely on the squash commit having its own CI run
or accept the latest successful workflow indiscriminately. If several associations
are ambiguous, fail rather than choose an unrelated proof. Release jobs need
`contents: read`, `actions: read`, `checks: read` for gate attribution, and
`pull-requests: read` for merged-PR association;
only pushing jobs additionally get
`packages: write`. There is no production secret or contents-write requirement.

Read-only repository API responses omit the administrative merge-method fields.
Verify squash-only PR protection through the public effective rules for main,
rather than requiring administrator access or assuming omitted settings.

Each architecture publisher loads its verified OCI archive using rootless Podman
and checks its configuration identity. Rootless Skopeo `copy --preserve-digests`
transfers the same OCI manifest/config/layers: Podman 5.4.2 has no preserve-digests
push option. Skopeo is installed from the disposable runner's Ubuntu repositories,
with its version logged. Version-specific component tags
`v<version>-amd64` and `v<version>-arm64` support assembly and are immutable too.
Preflight existing tags: a matching digest is idempotent, a different digest
fails. Do not execute the image or rerun the build/test suite during publishing.

After both component digests are verified, an unprivileged index job asserts
rootless Podman and publishes canonical OCI index JSON through the registry API.
Fixed descriptor order and annotations make the expected digest reproducible
before any write, without tool-generated timestamps or format conversions. Report the
expected index digest so retries cannot create a different index for an existing
version tag. The version tag is published only after both architectures exist.
Before advancing `latest`, compare the already published stable version using
semantic ordering. Do not downgrade, promote a prerelease, or overwrite an
unrecognized existing latest image without an explicit operator resolution.

Authenticate to `ghcr.io` using the job token over stdin and a temporary private
auth file; never log credentials or persist that file in an artifact. Derive
the lower-case package name from the configured target repository, initially
`ghcr.io/calmcacil/sonarr-remediator`. No PR publishes staging images to GHCR.

Initial package creation may require an owner setup step to make it public and
grant the recreated repository publishing access. Verify anonymous pulls after
the first release. An existing package may survive repository recreation; do
not delete it, overwrite immutable versions, or assume the new repository's
workflow token inherits its access. Anonymous verification downloads both platform
manifests and their config/layer blobs. Production activation is outside publishing.

### Fresh Repository Setup Order

1. Prepare a fresh repo with the approved rebuild specs/setup metadata as its
   initial root commit. Preserve old work externally only if the operator wants
   it; no legacy implementation is required by the new repo.
2. Set repository/Actions options and active main/tag rulesets. The required check
   may initially be pending until the first buildable CI/application PR emits it;
   lack of an emitted check is never a reason to merge an untested implementation.
3. Introduce the CI/buildable service scaffolding and checks through a normal PR.
   Verify the emitted check-run name/app ID and both runner platforms; bind or
   correct the check requirement before permitting its merge.
4. Verify protection with read-only API inspection and disposable controlled
   PR/rule checks: owner direct push and force/delete must be denied, successful
   current CI must permit a squash merge, and stale/failed CI must block it.
5. Configure GHCR access/visibility and verify an explicit test release's promotion
   without any production deployment or second CI suite.

The implementation now includes both workflows, evidence/release validation,
Renovate, and `.github/repository-policy.json` with a plan/apply/read-back script.
The script verifies a real emitted CI integration before applying rules and fails
on overlapping classic or unexpected inherited protection. Remote policy changes
and publication require operator authorization/setup. No repository recreation
or deletion is automated; deleting a repository requires explicit authorization.

## Testing and Validation

| Product invariants | Required validation during implementation |
| --- | --- |
| 1-3, 9 | One opened/synchronized PR run; no ordinary push/main/metadata duplicate; cancellation and explicit retry behavior |
| 4-6 | Formatting/vet/race/vulnerability failures; native amd64/arm64 identity; rootless engine plus non-root app; fake-Sonarr persistence/dry-run/shutdown smoke tests |
| 7-8 | Gate fails on failed/skipped/cancelled/missing prerequisites or artifacts; fork token cannot publish or use secrets |
| 10-16 | Read back repo/rules settings; zero approvals; owner subject to PR/check/up-to-date/linear/force/delete/tag protections; bootstrap cannot become a standing bypass |
| 17-20 | VERSION validation; exact post-test OCI identities; hash/receipt agreement; five-day expiry and attempt/repo/source mismatch rejection |
| 21-24 | Only explicit release triggers publishing; squash-tree equality; reject direct/unverified tags, wrong version, collisions, missing architecture, prerelease latest, and downgrade |
| 25-28 | Serialized partial publish and idempotent retry; package permissions/visibility; anonymous multi-platform pull; reported digests and no production side effects |

Implement scripts for Go checks, Podman smoke/export, receipt validation, and
release verification/promotion with deterministic failure paths. Workflow linting
and controlled fake API/registry responses validate orchestration; actual native
rootless runners and a disposable GitHub setup validate platform/rules behavior.
Passing YAML lint alone is not proof of rootless execution or enforced protection.

Local check results, native arm64 identities, current read-only remote settings,
and remaining hosted/rules/package acceptance are recorded in
[docs/CI.md](../../docs/CI.md). Record real hosted run IDs and policy read-back
when activating the implementation; local evidence is not a promotion receipt.

## Parallelization

Implement the first CI/rules/publisher sequence serially. Promotion consumes the
gate's exact artifact/evidence format, and protection depends on the emitted
check identity. Two native architecture jobs run concurrently inside one CI
workflow; no separate development agents or worktrees are proposed.

## Documentation References

- [GitHub-hosted runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
- [Workflow events](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows)
- [Available ruleset rules](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets)
- [Downloading workflow artifacts](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/download-workflow-artifacts)

## Operational Notes

- Five-day artifact retention intentionally makes release promotion time-bounded;
  expired artifacts require explicit recovery and never an automatic rebuild.
- The architecture component tags are implementation records; the primary public
  tags are immutable `v<version>` and stable `latest`.
