# CI, Repository Policy, and Publishing

This is the maintainer procedure. For local tool setup, checks, and image builds,
see [Building from source](building_from_source.md). Past acceptance results live
in [Historical validation evidence](VALIDATION.md), not in this procedure.
The approved contract is in the [product spec](../specs/github-actions/PRODUCT.md)
and [technical spec](../specs/github-actions/TECH.md).

## Pull Request CI

Opening or updating a PR against `main` runs `.github/workflows/ci.yml`, including
for drafts. Other PR events and ordinary branch/main pushes do not trigger it.
A newer revision cancels older work for the same PR.

The required check is `CI`. It requires `go`, `Podman (amd64)`, and
`Podman (arm64)`, then validates both OCI archives and creates a promotion receipt.
Native runners are `ubuntu-24.04` and `ubuntu-24.04-arm`. Runtime acceptance uses
fake Sonarr and private rootless namespaces, never production credentials.
Actions, mise, and tools are pinned. CI tests the PR merge ref with both parents
available; image version/revision labels must match `VERSION` and that checkout.

Promotion artifacts expire after **five days**:

- `image-amd64-<run>-<attempt>` and `image-arm64-<run>-<attempt>` contain
  `image.oci.tar`, `metadata.json`, and `runtime.json`.
- `receipt-<run>-<attempt>` contains `receipt.json`.

The receipt binds repository/workflow IDs, PR/head/base/checkout/tree, run/attempt,
version, gate outcomes, server artifact identities, image identities, and runtime
evidence. Missing, unknown, or mixed-attempt evidence fails the gate. When
recovering evidence, rerun the **entire original workflow**, not individual jobs.
A past successful check does not extend artifact retention.

## Repository Policy

Review `.github/repository-policy.json`, then run from the repository root:

```sh
mise run repo:plan
mise run repo:inspect
```

Inspection uses `GH_TOKEN`/`GITHUB_TOKEN` or the mise-managed GitHub CLI login.
It is read-only and fails if settings differ. An authorized administrator can
apply the reviewed settings with `mise run repo:apply`.

Apply requires a real emitted `CI` check from GitHub Actions app ID 15368. It
rejects overlapping classic protection and unexpected/inherited rulesets before
writing, then reads back the required fields. APIs are not transactional: after
an interruption, rerun inspection. Resolve inherited organization requirements
with the operator; do not add bypasses to make a merge possible.

The policy requires squash-only merging, automatic branch deletion, read-only
workflow tokens, SHA enforcement, the intentional `jdx/mise-action` allowlist,
and active main/tag rulesets with no bypass actors. Main requires a PR, current
successful `CI`, an up-to-date branch, resolved conversations, and linear history.
Independent approvals and extra approval for unattributed changes are zero.
Force updates and deletion are prohibited. `v*` tags may be created but not
updated or deleted. Repository recreation/deletion is not automated.

For policy activation or changes, confirm both hosted native jobs and the
emitted check/app, apply/read back settings, and exercise disposable owner
push/force/delete and stale/failed-PR checks without changing production refs.
An absent required check is blocking.

## Renovate

Enable the Renovate GitHub App for the repository. `renovate.json` manages Go
modules, the five mise tool pins, all `*Containerfile` inputs, Action SHAs, and
explicit mise runtime versions in workflows. Govulncheck releases are looked up
through `golang.org/x/vuln`, not its executable import path.

Go updates group mise's `golang/go`, the `go.mod` directive, and both Go builder
images into one PR; the contract test rejects drift. Other tools/actions have
focused groups. Keep image digests and full Action SHAs. Automerge is disabled;
updates rebase behind main and use normal CI and maintainer squash merge.
There is no scheduled test workflow or publisher token in PR CI.

Runner updates are intentionally manual: Renovate disables only the
`github-actions` manager's `github-runners` datasource for `ubuntu`. Changing
`ubuntu-24.04` or `ubuntu-24.04-arm` requires actionlint and native rootless Podman
acceptance on both architectures. Action and image updates remain enabled.

Run `mise run renovate:check` to validate configuration. It uses the official
release image pinned by tag and multi-platform digest in
`scripts/RenovateContainerfile`, with read-only root/config, no network,
capabilities, or credentials. Rootless Podman is required; there is no npm
fallback or trust-policy exemption.

## Publish a Release

1. Change `VERSION` in a PR and squash-merge only after CI and policy checks pass.
   The version has no leading `v` or build metadata.
2. While the final PR run's evidence remains available (five days), create the
   matching immutable `v<VERSION>` tag at the squash commit.
3. **Publish** a GitHub release for that tag. Set the prerelease flag for semantic
   prereleases such as `-rc.1`. A tag push, draft release, or main merge alone
   does not publish an image.
4. Confirm `.github/workflows/publish.yml` succeeds, including anonymous image
   pulls, before reporting publication success. Deployment is a separate operator
   action.

The read-only verification job checks main ancestry, merged PR association, the
current successful CI attempt/check, matching tested/squashed trees and PR merge
parents, and all artifact/runtime/image/version identities. Main's public
effective-rules API supplies squash-only protection evidence. Each write job
revalidates evidence. The publisher does not rebuild or run service tests.

Native jobs load the tested archives with rootless Podman and use Skopeo
`copy --preserve-digests` to transfer their original OCI bytes. Skopeo comes from
the hosted runner's Ubuntu repositories; its version is logged. Registry access
uses only the job token and a temporary mode-0600 auth file, never artifacts.

The index uses canonical OCI JSON in fixed amd64/arm64 order. Immutable
`v<VERSION>-amd64`, `v<VERSION>-arm64`, and `v<VERSION>` tags accept an existing
matching digest as a successful retry; conflicts fail without overwrite.
Publication is serialized without cancelling an in-progress release. Only a
completed stable version can advance `latest`; prereleases and older stable
versions cannot. A failed component leaves `latest` untouched.

Confirm publishing access to `ghcr.io/calmcacil/sonarr-remediator`. A package can
survive repository recreation; conflicting tags are not deleted or overwritten.
An unrecognized/legacy `latest` requires operator resolution. If necessary, the
owner must make the package public in GitHub package settings, then explicitly
retry while evidence remains available. The index job verifies anonymous
index/platform/config/layer pulls and reports the version digest, platforms,
tested PR/run/revision, and tagged squash commit.
