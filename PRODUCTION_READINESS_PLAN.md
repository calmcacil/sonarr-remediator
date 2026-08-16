# Production Readiness Plan — Sonarr Remediator

Status: code is complete and well-tested (go test/vet/`-race` green, 93–100%
coverage). Remaining work is one real feature gap, CI/release automation, and
a handful of spec-drift fixes. Findings below were verified directly against
the source on 2026-08-15.

## Priorities

### P0 — Blockers (gate "production testing" per SPEC §1/§12)

- **B1. Sonarr v4 import recovery / retry — FIXED (2026-08-16).**
  The parse-based pipeline (`GET /api/v3/parse`, which v4 answers 204 to
  `path=` calls) is removed. §3.4 recovery and §3.6 retries now run on the
  manual-import **preview** flow (`ManualImportPreview` +
  `ManualImportCommand` + queue-poll import proving), exactly like
  reconciliation — filesystem-independent and working on Sonarr v3 and v4.
  The media mount and the `paths` config keys are retained for compatibility
  only. Tests: unit + integration scenarios rewritten to the preview flow,
  including a positive v4 recovery scenario.

- **B2. No CI / release automation.**
  No `.github`/CI present. DOCKER_SPEC §7 defines an image-tag scheme
  (`latest` / `v1` / `v1.2` / `v1.2.0` / `@sha256`) but nothing builds or
  pushes multi-arch images. *Fix:* add a GitHub Actions workflow that on push
  runs `go vet`, `go test -race`, coverage; on tag/release builds
  `linux/amd64,linux/arm64` images via buildx and pushes to `ghcr.io` with
  semver tags + immutable digest. Also push `:latest`/`:dev` on `main`.

### P1 — Spec drift / correctness (small, testable)

- **C1. `reconcile.minimumConfidence` unimplemented.**
  `ReconcileConfig` (`config.go:127`) has only `Enabled`; SPEC §8 declares
  `minimumConfidence: 95`. A config copied from the spec is rejected as an
  unknown field. *Fix:* add the field, apply it in the reconcile safety gate.

- **C2. 401/403 auth failure not distinguished.**
  `ErrAuth` is defined/returned (`client.go:29,192`) but never consumed via
  `errors.Is`. SPEC §5.1 wants a distinct auth-failure event and monitors
  paused until credentials are fixed; today it logs generic
  `error.sonarr-unreachable`. *Fix:* detect `errors.Is(err, ErrAuth)` in the
  health monitor, log a dedicated `error.sonarr-auth` event, and keep the
  engine down until a successful probe.

- **C3. `startupDelay` is effectively dead.**
  `sonarrUp` defaults false (`engine.go:39`) and nothing sets it at startup,
  so the first queue poll waits for the first health tick (~60–90 s) regardless
  of `startupDelay`. *Fix:* call `engine.SetSonarrUp(true)` in `main.go`
  right after a successful `DetectVersion`.

- **C4. Stuck-download age gate hard-coded to 2 h.**
  `engine.go:28` `stuckMinAgeHours = 2.0` ignores
  `automation.removeBrokenDownloads.waitHours`. *Fix:* use the configured
  value (default 6 h).

### P2 — Polish

- **P2a. DONE (2026-08-16).** §3.2 "no manual import scheduled for this item"
  check implemented as the `manual_import.scheduled` engine gate (explicit in
  the decision log, backed by the same tracking maps as the duplicate/cooldown
  constraints).
- **P2b. DONE (2026-08-16).** §3.3 "no other active queue item for same
  episode" now enforced on the winning candidate in the queue monitor,
  covering both detection methods (queue message and history event) — it was
  previously only applied to Method A.
- **P2c. DONE (2026-08-16).** Dead `lastSeen` diff state removed from the
  queue monitor; SPEC §5.2 describes the full-evaluation-per-poll model.
- **P2d. DONE (2026-08-16).** In-flight healthcheck/Dockerfile/README/SPEC/
  DOCKER_SPEC work committed (`5845e40`).

## Recommended sequence

1. **Commit baseline** — commit the in-flight healthcheck + DOCKER_SPEC work.
2. **P1 fixes** (C1–C4) — each small and unit-testable; land behind existing
   test patterns.
3. **B2 CI** — build/verify on push; multi-arch push to ghcr on tag.
4. **B1 v4 recovery** — DONE: reworked onto the preview flow with a positive
   v4 mock scenario (SPEC §12).
5. **P2 polish** — drop dead state, add the two missing safety checks.
6. **Release** — cut `v1.0.0`; verify rootless-podman run + `--healthcheck`.
