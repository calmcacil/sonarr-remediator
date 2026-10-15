# Deployment

Use `ghcr.io/calmcacil/sonarr-remediator` on Linux amd64 or arm64. Published
multi-platform tags select the host architecture automatically. `latest` tracks
completed stable releases; prereleases do not advance it. Prefer a released
`v<VERSION>` tag or digest for reproducible deployments. Available versions are
listed in [GitHub releases](https://github.com/calmcacil/sonarr-remediator/releases).
No source checkout or compiler is required to run the image.

## Requirements

- One Sonarr v4 instance reachable from the container, with its API key.
- One observer per Sonarr instance, even if separate state volumes are available.
- Read-only configuration and a private, writable, persistent state directory.
- A non-root container running as UID/GID 65532, with a read-only root filesystem.

No ports, media directories, container-engine sockets, or host networking are
needed. All media operations go through Sonarr. HTTPS uses bundled trust roots
and normal certificate verification.

## Rootless Podman

Use a normal host account with working subordinate UID/GID mappings. Follow the
[README quick start](../README.md#quick-start). The
`--userns=keep-id:uid=65532,gid=65532` mapping lets that account own private bind
mounts without changing host ownership. Directories should be mode 0700 and
configuration/secret files mode 0600.

If Sonarr is on a private container network, add `--network YOUR_PRIVATE_NETWORK`
to the run command. Its configured hostname must resolve on that network;
`localhost` inside the container is not the host or another container.
On SELinux hosts, use private bind labels where required:
`-v "$PWD/config:/config:ro,Z" -v "$PWD/state:/state:Z"`.

Keep `--read-only`, dropped capabilities, `no-new-privileges`, and the 30-second
stop allowance. The health command is local and needs no published port.

## Rootless Docker Compose

Docker is a secondary compatibility path; native rootless Podman is the project's
runtime acceptance path. Save [compose.yaml](../compose.yaml) beside your `config`
directory. It pulls the published image and uses a named state volume initialized
from the image's private UID/GID 65532 directory.

Docker has no Podman keep-id mapping. A rootless daemon maps container UID 65532
to a subordinate host UID. Before starting, grant that mapped identity read access
to the config and secret files and traversal access to their directory using
host ACLs. Alternatively, provision a private configuration volume with mode-0600
files owned by container UID/GID 65532 and change the configuration mount to use
it. Do not make secrets world-readable. The application does not repair ownership.

Confirm `docker info` reports rootless mode, then run:

```sh
docker compose pull
docker compose up -d
docker compose exec remediator /sonarr-remediator status --json
docker compose logs --tail 50 remediator
```

Set the Compose `image` to your chosen release tag or digest before deployment.
If Sonarr uses another Compose project, attach both services to a shared private
network and configure a reachable Sonarr URL. No `ports` entry is needed.

## Persistent State

The default database is `/state/remediator.db`. Its directory and database,
lock, and WAL files must remain private. Keep the state mount when recreating
containers; do not use `docker compose down -v` for routine updates.

The store is bound to the canonical configured Sonarr URL. To change endpoints,
stop the old observer and deliberately choose a new store. Separate stores do
not coordinate concurrent observers. A second observer sharing a store is rejected.

Review [configuration](CONFIGURATION.md) before enabling mutations and
[upgrade and rollback guidance](UPGRADING.md) before changing image versions.
