# Sonarr Remediator

Recover completed downloads that Sonarr v4 cannot import. The service asks Sonarr
for automatic matches and requests imports only when every file passes policy.
It uses Sonarr's API only: no media mounts, inbound ports, or separate database.
Run one observer per Sonarr instance. Radarr and stalled/in-progress downloads
are outside its scope.

## Safety Defaults

- Background work starts in dry-run mode. Review decisions before enabling it.
- Cleanup, blocklisting, and replacement searches are disabled separately.
- Cleanup can delete downloaded data, including a whole pack. Permanent
  episode/folder mismatches can qualify immediately, without the cleanup wait.
- Manual `trigger --execute` authorizes one live cycle even during background
  dry-run; configured permissions and mutation limits still apply.

See [recovery and cleanup policy](docs/POLICY.md) before enabling deletion.

## Quick Start

Use the published image, `ghcr.io/calmcacil/sonarr-remediator`, for Linux amd64 or
arm64. The example uses `latest`; pin a [release tag](https://github.com/calmcacil/sonarr-remediator/releases)
(`v<VERSION>`) or digest for controlled updates.

For rootless Podman, create private configuration and state directories:

```sh
mkdir -m 700 config state
```

Save [config.example.yaml](config.example.yaml) as `config/config.yaml`. Set
`sonarr.url` to an address reachable from the container, and put your Sonarr API
key in `config/sonarr_api_key`. Leave `dry_run: true` while reviewing behavior.

```sh
chmod 600 config/config.yaml config/sonarr_api_key
podman run -d --name sonarr-remediator \
  --userns=keep-id:uid=65532,gid=65532 \
  --read-only --cap-drop=all --security-opt=no-new-privileges \
  --stop-timeout=30 --health-cmd='/sonarr-remediator healthcheck' \
  --health-start-period=3m \
  -v "$PWD/config:/config:ro" -v "$PWD/state:/state" \
  ghcr.io/calmcacil/sonarr-remediator:latest run --config /config/config.yaml
podman exec sonarr-remediator /sonarr-remediator status --json
podman exec sonarr-remediator /sonarr-remediator issues
```

Keep the state directory across container replacements. For private networks,
SELinux, Docker Compose, and secret permissions, follow the
[deployment guide](docs/DEPLOYMENT.md).

## Documentation

- [Configuration](docs/CONFIGURATION.md): credentials, overrides, scope and enabling recovery.
- [Operations](docs/OPERATIONS.md): inspection, manual cycles, logs, health and backups.
- [Policy](docs/POLICY.md): eligibility, waits, cleanup and uncertainty protection.
- [Upgrading](docs/UPGRADING.md): image updates, breaking changes and rollback.
- [Building from source](docs/building_from_source.md): development and local checks.
- [CI and releases](docs/CI.md): maintainer procedures; [historical validation](docs/VALIDATION.md).

Approved behavior and architecture are maintained in [specs](specs/sonarr-remediator/PRODUCT.md).
