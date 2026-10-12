# Initial Implementation Validation

The initial Sonarr Remediator service is implemented on `clean-rewrite` from the
approved clean-rewrite specifications. Validation uses anonymized evidence and
controlled Sonarr fixtures. No live Sonarr import, removal, blocklist, or search
was requested.

Development tools are pinned in `mise.toml`: Go 1.27.2, Python 3.14.8,
govulncheck 1.9.0, actionlint 1.7.11, and gh 2.102.0. Use `mise install` and
`mise run check` to reproduce the local checks. CI now installs and runs its
toolchain through mise. [CI validation and setup](CI.md) records release/receipt
checks, Renovate validation, and remaining hosted acceptance.

## Verified Locally

| Check | Result |
| --- | --- |
| Go 1.27.2 formatting, `go vet ./...`, `go test -race ./...` | Passed |
| All 14 captured queue/preview cases | Passed the documented expected policy matrix |
| Strict config, credentials, exclusions, dry-run boundary | Passed |
| Pagination consistency, resource presence/identity, response bounds, redirects | Passed |
| Eligibility gaps/restart, duplicate state owner, instance binding, read-only queries | Passed |
| Import command shape, known-failure spacing, exhaustion, correlated verification | Passed |
| Full multi-episode import with complete grabbed history and current-file evidence | Passed |
| Unusable removal flags, partial blocklist effects, continuation search across restart | Passed |
| Subprocess exit before and after transmission | Durable intent, budget, uncertainty, and lock release verified |
| Nine independently constructed review regressions | Retained in the repository; all passed |
| Vulnerability source checks for Linux amd64 and arm64, pinned `govulncheck` v1.9.0 | No reachable vulnerabilities found |
| Static Linux amd64 and arm64 binaries | Built; ELF architecture/static linking verified |
| OCI evidence validation and corrupted-layer/unsupported-platform/root-user rejection | Passed |
| GitHub workflow lint, pinned `actionlint` v1.7.11 | Passed |
| Secondary Docker Compose configuration | `docker compose config --quiet` passed |

The vulnerability checker also reported one finding in a required module that
is absent from imported packages and reachable calls. The checker exits cleanly
under the approved reachable-vulnerability gate.

## Native Rootless Podman

The final arm64 image was built and exercised under Podman 5.4.2, rootless UID
1000, overlay storage, with the non-root application UID/GID 65532. This host
has no systemd user session; the harness uses rootless cgroupfs and file events.

Sanitized results and exact image/source identities are recorded in
[runtime.json](validation/arm64/runtime.json),
[oci.json](validation/arm64/oci.json), and
[source-inputs.json](validation/arm64/source-inputs.json).

The harness checks non-root execution, private state, read-only root and config,
no inbound ports/media/socket mounts, zero dry-run mutations, local query output,
health, recommendation deduplication, persistence after recreation, bounded
shutdown, missing-config failure, and state-permission failure. It removes only
its own disposable containers/network/fixture image. OCI export is validated
against the tested configuration digest and every manifest/config/layer hash.

These local records identify the working-tree production inputs. PR CI records
the committed tree, repository, PR/run/attempt, native runtime evidence, and
archive identities in its receipt.

## Independent Review

A worker started with fresh context, read the approved specs and evidence,
reviewed every Go package and delivery input, checked pinned Sonarr source, and
constructed its own failing regressions. The implementation fixes all identified
safety issues: active competition, incomplete replacement policy inputs,
monotonic partial effects, partial effects preceding failed reads, missing queue
identity, returned downloads blocking search, verification deadlines on read
errors, finish-based retention, and preserved status heartbeat evidence.

The worker rechecked the fixes and found no remaining blocking safety regression.
Its final minor repeated-state logging finding is also fixed. The review's
additional valid-pack and crash-boundary coverage is now present and passing.

## Release Acceptance

Native amd64 container runtime acceptance remains required before release. The
local host is arm64; successful amd64 compilation establishes a build target.
`.github/workflows/ci.yml` supplies native amd64 and arm64 rootless Podman jobs,
source vulnerability checks for both targets, exact OCI artifacts, and an
always-evaluated `CI` gate that rejects failed/missing/mixed-attempt evidence.
The workflow has been linted locally; hosted runs have not been requested.

The separately approved GitHub Actions specification now has checked-in
repository-policy setup/read-back tooling and immutable release promotion.
Hosted execution, policy enforcement, and actual publication remain operational
acceptance steps. No remote repository settings, release tags, GHCR package,
or production deployment were changed during local implementation.
Rootless Docker runtime acceptance is secondary and has not been claimed from
the Compose configuration check.
