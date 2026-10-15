# Historical Validation Evidence

This archive records acceptance reported during the initial clean rewrite and
CI rollout. It is not a verification report for the current checkout or release.
Tool versions, counts, timings, repository settings, and package state below are
historical observations, not current guarantees. No checks were rerun for this
documentation reorganization.

For current procedures, use [Building from source](building_from_source.md) and
[CI, repository policy, and publishing](CI.md). Each release needs its own valid
CI evidence; these records cannot replace a promotion receipt.

## Initial Clean-Rewrite Acceptance

The initial service was implemented on `clean-rewrite` from the approved
clean-rewrite specifications. Validation used anonymized evidence and controlled
Sonarr fixtures. No live Sonarr import, removal, blocklist, or search was requested.
The original report named Go 1.27.2, Python 3.14.8, govulncheck 1.9.0,
actionlint 1.7.11, and gh 2.102.0. Current pins belong to `mise.toml`.

### Local Results Reported

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

### Native Rootless Podman

The initial arm64 image was built and exercised under Podman 5.4.2, rootless UID
1000, overlay storage, with the non-root application UID/GID 65532. That host
had no systemd user session; the harness used rootless cgroupfs and file events.

Sanitized results and exact image/source identities are recorded in
[runtime.json](validation/arm64/runtime.json),
[oci.json](validation/arm64/oci.json), and
[source-inputs.json](validation/arm64/source-inputs.json).

The harness checked non-root execution, private state, read-only root and config,
no inbound ports/media/socket mounts, zero dry-run mutations, local query output,
health, recommendation deduplication, persistence after recreation, bounded
shutdown, missing-config failure, and state-permission failure. It removed only
its own disposable containers/network/fixture image. OCI export was validated
against the tested configuration digest and every manifest/config/layer hash.

These local records identify the working-tree production inputs. PR CI records
the committed tree, repository, PR/run/attempt, native runtime evidence, and
archive identities in its receipt.

### Independent Review

A worker started with fresh context, read the approved specs and evidence,
reviewed every Go package and delivery input, checked pinned Sonarr source, and
constructed its own failing regressions. The report states that fixes addressed
safety issues: active competition, incomplete replacement policy inputs,
monotonic partial effects, partial effects preceding failed reads, missing queue
identity, returned downloads blocking search, verification deadlines on read
errors, finish-based retention, and preserved status heartbeat evidence.

The worker rechecked the fixes and reported no remaining blocking safety
regression. Its final minor repeated-state logging finding was also fixed.
Additional valid-pack and crash-boundary coverage was reported present and passing.

### Acceptance Boundaries

The local host was arm64; amd64 compilation established a build target, not native
runtime acceptance. The first hosted trial subsequently passed both native
architectures (recorded below). Every new revision still needs its own successful
run and matching promotion receipt. No production deployment was requested.
Rootless Docker runtime acceptance was secondary and was not established by
the Compose configuration check.

## Initial CI Implementation: Local Evidence

The original CI implementation report recorded a passing full mise check,
30 CI/release regressions (also under Python optimization), and native arm64
Podman 5.4.2 acceptance under UID 1000 with overlay storage. Shutdown completed
in 0.216 seconds. Read-only config/private writable state were inspected, and
OCI export/import preserved manifest, config, and layers. The revision label
was checked against `0b51f62132df0364dd6cb103c1e90cc9369ee636`. This was local
working-tree validation, not a GitHub promotion receipt. The report referenced
ignored `artifacts/ci-local-validation.json`; that path is not a tracked evidence
artifact.

The registry client performed real anonymous reads against the pinned upstream
Renovate GHCR index and arm64 manifest/config blob, verifying digests without
publishing. GitHub download tests rejected corrupt ZIPs, unexpected files/hosts,
and forwarding the workflow token to artifact storage.

An extraction-only upstream Renovate run detected all five mise pins, all three
Containerfiles, both workflows, and all six mise runtime versions. Its native
`github-actions` manager handled runtime inputs without custom regex. The Go
group included native mise identity `golang/go`; the other mise group excluded
it. Extraction used no token; GitHub lookups required normal App authentication.

Two implementation observations informed the checked-in tooling:

- Installing the Renovate npm release through mise failed its provenance-downgrade
  policy for `@yarnpkg/libzip@3.2.2`. The validator used the official pinned image,
  without a trust-policy exemption or npm fallback.
- Podman 5.4.2 reused cached LABEL values after revision build-argument changes,
  motivating cache-free production builds. Its push interface could not demand
  digest preservation, motivating Skopeo for promotion.

Before activation, repository ID 1415679277 was public, with default main and
read-only workflow tokens, but allowed all merge methods/actions and had no
rulesets. Local implementation did not change those remote settings.

## Initial Hosted Acceptance (2026-10-12)

[PR #5](https://github.com/calmcacil/sonarr-remediator/pull/5)'s first revision
passed [CI run 38280473491](https://github.com/calmcacil/sonarr-remediator/actions/runs/38280473491),
attempt 1: Go, both native Podman jobs, and the strict CI gate succeeded. Both
runners used rootless Podman 4.9.3, UID 1001, and overlay storage. Shutdown took
0.093 seconds on amd64 and 0.079 seconds on arm64. The receipt ZIP was downloaded
and verified against its GitHub artifact digest.

That receipt recorded head `a4f43f917a311f5c4cd8cac1af12637048cce27c`, tested
checkout `1eb2e21d0bef11c3faa303b050172ebc63ab16b8`, tree
`32a1e427f28bc04b68ca76c19f08913d9b9bad06`, and VERSION 0.1.0. It established
the first hosted trial, not promotion eligibility for any later revision.

Reviewed settings were applied and read back with no mismatches: squash-only
merges, branch deletion, read-only token defaults, selected SHA-pinned actions,
main ruleset 24940631, and immutable-tag ruleset 24940633. Both rulesets reported
`current_user_can_bypass: never`. Required CI was bound to Actions ID 15368 with
strict up-to-date checks, zero approvals, and no extra unattributed-change
approval. **No destructive main/tag probes were performed.**

Renovate onboarding was merged and its App produced dependency PRs. The operator
explicitly requested and performed deletion of the surviving legacy GHCR package;
a read-only API lookup confirmed absence before the fresh 0.1.0 release. This
historical operator action is not an automated collision-recovery procedure.

The original report referred readers to the PR and release workflow for the
final-revision CI and initial public release results; it did not include a
specific release-run link here. Public package setup and anonymous pulls remained
required before claiming publication success. Local lint did not establish
publication, and this archive makes no new publication claim.
