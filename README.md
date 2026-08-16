# Sonarr Recovery Agent

A sidecar microservice that runs alongside Sonarr and automatically detects,
analyzes, and recovers from common download and import issues that normally
require manual intervention: stuck downloads, "not a custom format upgrade"
removals, and failed imports.

It interacts only with the Sonarr API. It has no HTTP server, no metrics
endpoint, and no notifications. Structured JSON logs on stdout are the only
interface; view them with `docker logs sonarr-remediator`.

## Dry run

Dry-run mode is enabled by default (`dryRun: true`). While active, the agent
detects and evaluates issues but never mutates anything; every action it
*would* take is logged as `action.recommended`. Disable it with
`SRA_DRY_RUN=false` or by editing the config file.

## Quick start (Docker Compose)

1. Copy `docker-compose.example.yaml` and adjust the volumes for your setup.
2. Create the config directory, copy the example config, and make it
   readable by the runtime user (`PUID`/`PGID`, default 1000:1000):

   ```
   mkdir -p remediator-config
   cp config.example.yaml remediator-config/config.yaml
   chown -R "${PUID:-1000}:${PGID:-1000}" remediator-config
   ```

3. Set the Sonarr API key, either in the environment (`SONARR_API_KEY`) or
   directly on the `SRA_SONARR__API_KEY` line of the compose file.
4. Start: `docker compose up -d`

The container mounts the shared media directory read-only at `/data` and the
config directory read-only at `/config`. The whole container runs read-only;
`--healthcheck` verifies configuration and Sonarr connectivity (`docker ps`
shows the container's health).

## Configuration

The agent is configured with a YAML file (see `config.example.yaml` for the
full schema). Any key can be overridden with the `SRA_` prefix and double
underscores for nesting:

```
SRA_SONARR__URL=http://sonarr:8989
SRA_SONARR__API_KEY=abc123
SRA_DRY_RUN=false
SRA_LOGGING__LEVEL=debug
SRA_AUTOMATION__AUTO_MANUAL_IMPORT__ENABLED=true
```

## Building

`make build` builds the `sonarr-remediator` binary; `make docker-build`
builds the container image.

## Reference

- [SPEC.md](SPEC.md) — application behavior: detection, safety checks, actions, configuration schema, action log.
- [DOCKER_SPEC.md](DOCKER_SPEC.md) — container packaging, Compose deployment, image tags, and lifecycle.
