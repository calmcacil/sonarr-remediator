# CI, Releases, and Dependency Updates

The approved contract lives in [the CI product spec](../specs/github-actions/PRODUCT.md)
and [technical spec](../specs/github-actions/TECH.md).

## Local Checks

Install mise and the host's rootless Podman packages, then prepare the project:

```sh
mise install
mise run check
```

`check` validates Renovate configuration, the shared Go patch version, formatting,
vet, race tests, CI/release regression tests, both workflow files, and reachable
Go vulnerabilities on Linux amd64 and arm64. `mise run ci:test` runs the focused
CI/release tests. `GOTOOLCHAIN=local` prevents an implicit Go upgrade.

Renovate's validator runs from its official release image, pinned by tag and
multi-platform digest in `scripts/RenovateContainerfile`. The container has a
read-only root and config mount, no network, no capabilities, and no credentials.
Installing the npm release through mise failed its provenance-downgrade policy
for `@yarnpkg/libzip@3.2.2`; no trust-policy exemption or npm fallback is used.
Podman is required for this local check as well as application acceptance.

## Pull Request CI

Opening or updating a PR against `main` starts one workflow. Draft PRs run the
same gates. Ordinary branch/main pushes, reopening, metadata changes, and marking
ready do not launch extra test runs. A new revision cancels older work for its PR.

The stable required check is `CI`. It requires `go`, `Podman (amd64)`, and
`Podman (arm64)`, then independently checks both archives and produces a receipt.
The Podman jobs use native `ubuntu-24.04` and `ubuntu-24.04-arm` runners. Runtime
checks use fake Sonarr, private rootless namespaces, and no production credentials.
All actions are SHA-pinned; mise itself and its runtimes/checkers are pinned.

Runtime promotion evidence includes manual preview, one-off execution during
background dry-run, correlated verification, receipt persistence, exit codes and
timeout checks. A source regression requires the smoke harness and promotion
validator to name the same complete set. Missing or unknown checks block promotion.

CI tests the PR merge ref. Checkout depth two preserves both parents for verifying
the event's head and base. Cache-free production builds avoid a demonstrated
Podman 5.4.2 bug that reused cached LABEL values after revision build-argument
changes. Image source/version labels must match the checkout and VERSION.

The only promotion artifacts are:

- `image-amd64-<run>-<attempt>` and `image-arm64-<run>-<attempt>`, each containing
  `image.oci.tar`, `metadata.json`, and `runtime.json`.
- `receipt-<run>-<attempt>`, containing `receipt.json`.

All expire after five days. The receipt records the GitHub repository/workflow
IDs, run/attempt, PR/head/base/checkout/tree, version, named gate outcomes,
server artifact IDs/names/digests/times, image identities, and runtime evidence.
Partial retries cannot combine attempts. Retry the **entire** original workflow
when recovering evidence; successful historical CI does not extend retention.

## Repository Policy Setup

The desired settings are reviewable in `.github/repository-policy.json`:

```sh
mise run repo:plan
mise run repo:inspect
```

Inspection uses the current `GH_TOKEN`/`GITHUB_TOKEN`, or the mise-managed GitHub
CLI login, and exits unsuccessfully if settings differ. It does not change them.
When authorized to enforce the reviewed settings, run:

```sh
mise run repo:apply
```

The apply command requires administrator access and a real emitted `CI` check
whose app is GitHub Actions ID 15368. It checks for overlapping classic protection
and unexpected/inherited rulesets before writing. It applies squash-only merging,
automatic branch deletion, read-only workflow tokens, the intentional
`jdx/mise-action` allowlist, SHA enforcement, and active main/tag rulesets with no
bypass actors. It then reads back every required field. API errors fail visibly;
settings APIs are not transactional, so rerun inspection after an interrupted apply.

Main requires a PR, successful current `CI`, an up-to-date branch, resolved review
conversations, and linear history. Independent approvals are zero; no explicit
reviewers or extra approval for unattributed changes are required. Owner bypass,
force updates, and deletion are prohibited. `v*` tag creation is permitted;
updates/deletion are prohibited. No remote recreation or deletion is automated.

Before merging implementation, verify both hosted native jobs and the emitted
check/app, apply/read back policy, and exercise disposable owner push/force/delete
and stale/failed-PR checks. An absent check is blocking; never add a bypass to
merge it. An inherited organization requirement needs operator resolution.

## Renovate

Install/enable the Renovate GitHub App for this repository. The checked-in
`renovate.json` uses its native `mise` manager for Go, Python, govulncheck,
actionlint, and gh. It also updates Go modules, all `*Containerfile` inputs,
GitHub Action SHAs, and the explicit mise runtime pins in workflow files.
Govulncheck looks up releases through its parent module `golang.org/x/vuln`;
the executable import path is not itself a versioned module in the Go proxy.

Go updates group mise, the `go.mod` directive, and both Go builder images into
one PR. The local/CI contract test rejects drift. Other mise tools and actions
have focused update groups. Images retain digest pins, actions retain full SHAs,
automerge is disabled, and updates rebase when behind main. Renovate PRs use the
same required CI and maintainer squash merge as other changes; there is no new
scheduled CI workflow or publisher token in PR CI.

An extraction-only upstream Renovate run detected all five mise pins, all three
Containerfiles, both workflows, and all six mise runtime versions. Its native
`github-actions` manager handles the runtime inputs; no custom regex is needed.
The Go group includes the native mise package identity `golang/go`, and the
other mise group explicitly excludes it. This local extraction used no token;
GitHub lookups require the installed Renovate App's normal authentication.

## Publishing

Change VERSION through a PR and merge only after CI and policy checks succeed.
Within five days, create the matching immutable `v<VERSION>` tag at its squash
commit and **publish** a GitHub release. A tag push, draft release, or main merge
alone does not publish. Set the release prerelease flag for a `-rc.1` or other
semantic prerelease. Versions cannot contain a leading `v` or build metadata.

The read-only verification job resolves the tag, checks main ancestry and the
merged PR association, then requires the current successful CI attempt and its
GitHub Actions check. The tagged squash tree must equal the recorded tested merge
tree, and the recorded merge parents must equal the PR base/head. All artifact,
archive, runtime, image, and version identities are revalidated before each write
job. The publisher does not build, execute images, or run service tests.
Squash-only protection is verified through main's public effective-rules API;
read-only repository views omit the administrative merge-method settings.

Native publisher jobs load archives with rootless Podman and verify image config
identities. Rootless Skopeo `copy --preserve-digests` transfers original OCI bytes;
Podman 5.4.2 cannot demand digest-preserving pushes. Skopeo is installed from the
hosted runner's Ubuntu repositories and its version is logged. Registry credentials
use only the job token and a temporary mode-0600 auth file, never artifacts.

The index job creates canonical OCI JSON in fixed amd64/arm64 order. Its digest
is known before any write. `v<VERSION>-amd64`, `v<VERSION>-arm64`, and `v<VERSION>`
accept an existing matching digest as a successful retry; any other digest fails
without overwrite. Registry API writes publish the canonical index bytes so
Podman-generated annotations/timestamps cannot change its retry identity.

Only a completed stable version may advance `latest`. Prereleases and older
stable versions cannot move it. Existing latest must identify the same repository
incarnation and match its immutable stable version; unrecognized/legacy latest
requires operator resolution. Publication is serialized without cancelling an
in-progress release. A failed component leaves latest untouched; retries reuse
available evidence and immutable matching components.

Before the first release, confirm the recreated repository has publishing access
to `ghcr.io/calmcacil/sonarr-remediator`. An older package can survive recreation;
this implementation neither deletes it nor overwrites conflicting tags. The first
package may require owner action to make it public in GitHub's package settings.
The index job verifies anonymous index/platform and config/layer pulls and reports
the version digest, platforms, tested PR/run/revision, and tagged squash commit.
A visibility failure requires owner setup and explicit retry while evidence is
available. Production deployment remains an operator action.

## Validation Evidence and Remaining Acceptance

Local implementation validation passed the full mise check, 30 CI/release
regressions (also under Python optimization), and native arm64 Podman 5.4.2 acceptance under UID 1000 with overlay
storage. Shutdown completed in 0.216 seconds. Read-only config/private writable
state were inspected, and OCI export/import preserved manifest, config, and layers.
The revision label was explicitly checked against commit
`0b51f62132df0364dd6cb103c1e90cc9369ee636`; this is local working-tree validation,
not a GitHub promotion receipt. The local JSON is in ignored
`artifacts/ci-local-validation.json`.

The registry client also performed real anonymous reads against the pinned
upstream Renovate GHCR index and arm64 manifest/config blob, verifying their
digests without publishing anything. GitHub download tests reject corrupt ZIPs,
unexpected files/hosts, and forwarding the workflow token to artifact storage.

Before activation, repository ID 1415679277 was public with default main and
read-only workflow tokens, but allowed all merge methods/actions and had no
rulesets. Local implementation did not change those remote settings.

## Initial Hosted Acceptance (2026-10-12)

[PR #5](https://github.com/calmcacil/sonarr-remediator/pull/5)'s first revision
passed [CI run 38280473491](https://github.com/calmcacil/sonarr-remediator/actions/runs/38280473491),
attempt 1: Go, both native Podman jobs, and the strict CI gate all succeeded.
Both runners used rootless Podman 4.9.3, UID 1001, and overlay storage. Native
shutdown took 0.093 seconds on amd64 and 0.079 seconds on arm64. The receipt ZIP
was downloaded and verified against its GitHub artifact digest.

The first receipt records head `a4f43f917a311f5c4cd8cac1af12637048cce27c`, tested
checkout `1eb2e21d0bef11c3faa303b050172ebc63ab16b8`, tree
`32a1e427f28bc04b68ca76c19f08913d9b9bad06`, and VERSION 0.1.0. This historical
receipt proves the first hosted trial; promotion must use the final merged PR
revision's own successful run and artifacts.

The reviewed settings were applied and read back with no mismatches: squash-only
merges, branch deletion, read-only token defaults, selected SHA-pinned actions,
main ruleset 24940631, and immutable-tag ruleset 24940633. Both rulesets report
`current_user_can_bypass: never`. Required CI is bound to Actions ID 15368 and
uses strict up-to-date checks, zero approvals, and no extra unattributed-change
approval. No destructive main/tag probes were performed.

Renovate onboarding is merged and its App is producing dependency PRs. The
operator explicitly requested deletion of the surviving legacy GHCR package;
they removed it, and a read-only API lookup confirmed absence before the fresh
0.1.0 release. Future version collisions still fail without overwrite.

Current-revision CI and initial public release results are recorded on the PR
and release workflow. Public package setup and anonymous pulls remain required
before reporting publication success; local lint does not establish them.
