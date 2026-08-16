# Docker Spec: Sonarr Recovery Agent (sidecar container)

This specification covers everything container-specific about Sonarr Recovery
Agent: how it is built, how it runs, how it is deployed next to Sonarr, and
how it is updated. Application behavior — detection, safety, actions, config
schema, action log — lives in [SPEC.md](SPEC.md). The two documents share the
same contract: no production testing until all features described here are
implemented.

The packaging follows the conventions of the sibling sidecar project
[`calmcacil/sonarr-anime-bridge`](https://github.com/calmcacil/sonarr-anime-bridge):
multi-arch build, rootless distroless runtime, immutable image digests, tag
tracks, and external update/rollback.

---

## Table of Contents

1. [Runtime Contract](#1-runtime-contract)
2. [Relationship with Sonarr](#2-relationship-with-sonarr)
3. [Image](#3-image)
4. [CLI](#4-cli)
5. [Volumes & Permissions](#5-volumes--permissions)
6. [Docker Compose Composition](#6-docker-compose-composition)
7. [Image Tags & Updates](#7-image-tags--updates)
8. [Lifecycle & Graceful Shutdown](#8-lifecycle--graceful-shutdown)
9. [Hardcoded Container Values](#9-hardcoded-container-values)

---

## 1. Runtime Contract

| Property | Value |
|---|---|
| Role | Sidecar container running beside Sonarr in the same Compose project/network |
| Interface | None inbound — no HTTP server, no ports, no metrics endpoint; stdout JSON logs are the only surface (`docker logs`) |
| State | Stateless container — all state is in memory; nothing is written anywhere |
| Filesystem | Read-only: the container root FS and both mounts are read-only |
| Runtime user | Rootless non-root (image default 65532:65532; Compose overrides with `PUID`/`PGID`) |
| Shell | None — the runtime image has no shell, package manager, or `su-exec` |
| Self-update | Never — image digests are immutable; updates are driven externally |
| Health | `--healthcheck` mode probes config validity + Sonarr connectivity (no HTTP endpoint exists to probe) |

Because the agent writes nothing, the container is fully read-only: root
filesystem `read_only: true` in Compose, both mounts read-only, no `VOLUME`,
no writable runtime path. There is nothing to back up or migrate.

## 2. Relationship with Sonarr

Sonarr is the microservice this sidecar depends on. The agent consumes only
Sonarr's REST API (`http://sonarr:8989` on the Compose network) and reads
media files from the shared data mount. It never talks to download clients
directly; Sonarr forwards queue removals.

Two mount contracts govern the sidecar relationship (SPEC §1 "Path
Translation"):

| Mount | Host example | Container path | Mode | Purpose |
|---|---|---|---|---|
| Media root | `./data` | `/data` | `ro` | Retained for compatibility; recovery is filesystem-independent (SPEC §3.4) and the agent reads nothing from it |
| Config | `./remediator-config` | `/config` | `ro` | `config.yaml` (config schema: SPEC §8) |

Both mounts are read-only — the agent never writes to either. Recovery and
retry flows use only Sonarr's manual-import preview/command endpoints, so the
agent does not need the media mount; it is kept because existing deployments
provide it and dropping it would break compatibility.

Sibling sidecars (e.g. `sonarr-anime-bridge`, which serves import lists *to*
Sonarr) may run in the same project; they share the same Sonarr instance but
have no interaction with this agent. See §6.

## 3. Image

### Build

Multi-arch (linux/amd64, linux/arm64) via `docker buildx`:

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS TARGETARCH
ARG VERSION=dev

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags="-s -w -X main.version=${VERSION}" -o /sonarr-remediator ./cmd/sonarr-remediator

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /sonarr-remediator /sonarr-remediator

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/sonarr-remediator", "--healthcheck", "--config", "/config/config.yaml"]

ENTRYPOINT ["/sonarr-remediator"]
CMD ["--config", "/config/config.yaml"]
```

- Builder is pinned to the Go toolchain version used by `go.mod` (1.26 line).
- `CGO_ENABLED=0` for a static binary; the runtime is distroless `static` (no
  glibc needed).
- Runtime image `static-debian13:nonroot` ships CA certificates (HTTPS Sonarr
  endpoints) and runs as UID/GID 65532 by default.
- `tzdata` is intentionally absent; the agent logs UTC timestamps only.
- No `EXPOSE`, no `VOLUME` — there is nothing to expose and nothing writable.
- The `VERSION` build arg is stamped into `main.version` and logged at
  startup, so `docker logs` identifies the exact build.

### Build context hygiene

`.dockerignore` excludes prebuilt binaries (`sonarr-remediator`,
`sonarr-remediator-amd64`), local configs that may contain API keys
(`config.local.yaml`, `config.live.yaml`), test sources, and markdown. The
build context is source + module files only.

## 4. CLI

| Flag | Default | Purpose |
|---|---|---|
| `--config <path>` | `config.example.yaml` | YAML configuration file (schema: SPEC §8) |
| `--healthcheck` | `false` | Validate config, probe Sonarr (`GET /api/v3/system/status`), exit 0/1. Used by the container HEALTHCHECK; probes with a 4 s internal deadline so it always finishes inside Docker's 5 s timeout |

`--healthcheck` performs no other work: no monitors start, nothing is
mutated. Exit 0 = config valid + Sonarr reachable. Any failure (config error,
auth failure, unreachable Sonarr) exits 1, which Docker reports as
`unhealthy`.

## 5. Volumes & Permissions

The runtime image starts as UID/GID 65532 (distroless nonroot). Compose sets
`user: "${PUID:-1000}:${PGID:-1000}"`, the same convention as
`sonarr-anime-bridge`. `PUID`/`PGID` are Docker/Compose variables only — not
application environment variables.

Bind mounts must be readable by the selected runtime UID/GID before the
container starts:

```bash
mkdir -p remediator-config
cp config.example.yaml remediator-config/config.yaml
chown -R "${PUID:-1000}:${PGID:-1000}" remediator-config
```

`/data` is typically owned by the user who runs the `linuxserver/sonarr`
container (default 1000:1000), so no special handling is needed when the same
`PUID` is used for both services.

## 6. Docker Compose Composition

### Minimal — Sonarr + remediator

```yaml
services:
  sonarr:
    image: linuxserver/sonarr:latest
    volumes:
      - ./sonarr/config:/config
      - ./data:/data
    healthcheck:
      test: ["CMD-SHELL", "wget -q --spider http://127.0.0.1:8989/ping || exit 1"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 30s

  sonarr-remediator:
    image: ghcr.io/calmcacil/sonarr-remediator:latest
    user: "${PUID:-1000}:${PGID:-1000}"
    read_only: true
    volumes:
      - ./data:/data:ro
      - ./remediator-config:/config:ro
    environment:
      - SRA_SONARR__URL=http://sonarr:8989
      - SRA_SONARR__API_KEY=${SONARR_API_KEY}
      - SRA_DRY_RUN=true
    depends_on:
      sonarr:
        condition: service_healthy
    restart: unless-stopped
    stop_grace_period: 30s
```

Notes:

- `SRA_SONARR__URL` resolves the Sonarr service name on the Compose network;
  the agent's own connectivity backoff handles brief blips after startup.
- `SRA_DRY_RUN=true` is the safe default (SPEC §3.8); flip to `false` only
  after observing dry-run output.
- `condition: service_healthy` waits for Sonarr's `/ping`; deployments whose
  Sonarr image lacks `wget` may drop the Sonarr healthcheck block and rely on
  `restart: unless-stopped` (startup retries via container restart).
- `stop_grace_period: 30s` matches the shutdown drain window (§8).

### With a sibling sidecar

The agent runs side by side with other sidecars that serve the same Sonarr
instance. `sonarr-anime-bridge` is the sibling pattern:

```yaml
services:
  sonarr:            # the shared microservice
    # ... as above

  sonarr-remediator: # consumes Sonarr's API
    # ... as above

  sonarr-seasonal:   # serves Sonarr's import lists (sonarr-anime-bridge)
    image: ghcr.io/calmcacil/sonarr-anime-bridge:v2
    user: "${PUID:-1000}:${PGID:-1000}"
    ports:
      - "8080:8080"
    volumes:
      - "${APPDATA_DIR:-./appdata/sonarr-anime-bridge}:/data"
    healthcheck:
      test: ["CMD", "/server", "--healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

The two sidecars do not talk to each other; both depend on the same Sonarr.

## 7. Image Tags & Updates

Published images are immutable for a given digest. The container never
downloads or replaces its binary at startup, keeping rollbacks, debugging,
and repeated deployments predictable.

| Image reference | Meaning | Use when |
|---|---|---|
| `ghcr.io/calmcacil/sonarr-remediator:latest` | Newest stable release; mutable | Simplest update path |
| `ghcr.io/calmcacil/sonarr-remediator:v1` | Latest v1 release; mutable | Major-line updates |
| `ghcr.io/calmcacil/sonarr-remediator:v1.2` | Latest v1.2 patch; mutable | Patch updates only |
| `ghcr.io/calmcacil/sonarr-remediator:v1.2.0` | Exact release tag | Reproducible deploys and rollback |
| `ghcr.io/calmcacil/sonarr-remediator@sha256:<digest>` | Exact image digest | Maximum reproducibility |

Recommended update approaches are external to the container: manual
(`docker compose pull && docker compose up -d`), Watchtower, Renovate or
Dependabot, or GitOps automation. To roll back, pin the previous exact tag.

## 8. Lifecycle & Graceful Shutdown

The agent handles `SIGTERM`/`SIGINT` per SPEC §11: monitors stop, in-flight
actions drain for up to 30 s, the decision ring buffer is flushed to stdout,
then the process exits 0. Compose obligations:

- `stop_grace_period: 30s` (a shorter window risks aborting in-flight
  imports; a second signal forces immediate exit).
- `restart: unless-stopped` recovers the sidecar after crashes and across
  host reboots.
- On exit, stdout/stderr are complete: every action and flushed decision was
  already emitted as a structured log line (SPEC §3.9).

## 9. Hardcoded Container Values

| Parameter | Value |
|---|---|
| Exposed ports | none |
| HEALTHCHECK interval / timeout / start-period / retries | 30 s / 5 s / 15 s / 3 |
| Healthcheck probe deadline | 4 s internal |
| Config path | `/config/config.yaml` |
| Media mount | `/data` (`ro`, shared with Sonarr) |
| Graceful shutdown drain | 30 s (`stop_grace_period: 30s`) |
| Runtime user | distroless nonroot 65532:65532 (image) / `PUID:PGID` (Compose) |
| Timezone | UTC only (no tzdata) |
| CA certificates | bundled (distroless static) |
| Log output | stdout, JSON (SPEC §9) |

---

*End of Docker specification.*
