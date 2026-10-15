# Building from Source

This guide is for contributors and maintainers. For normal deployment, use the
published GHCR image and the [README](../README.md). Local builds and smoke output
are not release promotion evidence; see [CI and publishing](CI.md).

## Prepare the Toolchain

Run commands from the repository root on Linux. Install Git, Bash, a C compiler
for Go race tests, and [mise](https://mise.jdx.dev/getting-started.html). For image
builds and the full check, install your distribution's rootless Podman packages,
including subordinate UID/GID mapping and rootless networking support. Use an
unprivileged account with working `/etc/subuid` and `/etc/subgid` ranges, writable
container storage, and access to pull the pinned upstream images and Go modules.
Do not run the smoke harness as root.

Review `mise.toml` before trusting its executable configuration:

```sh
mise trust
mise install
```

The file pins Go, Python, govulncheck, actionlint, and GitHub CLI. Use those pins
rather than installing separate versions. `GOTOOLCHAIN=local` prevents implicit
Go upgrades. `mise run` and `mise exec --` load tools and environment without
requiring shell activation; see the [mise command guidance](https://mise.jdx.dev/dev-tools/shims.html).

## Build Binaries and Run Checks

```sh
mise run build
```

This writes the native static binary to `bin/sonarr-remediator`. The task disables
CGO and VCS stamping; it does not inject the release version. For a versioned
Linux binary matching `VERSION`, build explicitly (choose `amd64` or `arm64`):

```sh
architecture=arm64
version=$(cat VERSION)
CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" mise exec -- go build \
  -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$version" \
  -o "bin/sonarr-remediator-linux-$architecture" ./cmd/sonarr-remediator
```

Cross-compilation does not establish native runtime acceptance.

| Command | Scope |
| --- | --- |
| `mise run check` | Renovate configuration, Go patch-version consistency, formatting, vet, race tests, Python OCI/CI/release regressions, workflow lint, and reachable Go vulnerabilities for Linux amd64/arm64 |
| `mise run test` | Go regression/integration tests with race detection |
| `mise run ci:test` | Python OCI evidence, CI/release, policy, and toolchain regressions |
| `mise run renovate:check` | Official pinned Renovate validator in rootless Podman |

The full check requires Podman even if you are only changing Go code. It does
not run native application image acceptance; that is a separate step below.

## Build and Smoke-Test a Native Rootless Image

Use a native Linux amd64 or arm64 host. This path uses a disposable fake Sonarr,
not your production configuration, credentials, downloads, or media. Podman must
support rootless private networks and `--userns=keep-id:uid=65532,gid=65532`.
The commands use cgroupfs and file events, as the harness does; a systemd user
session is not required.

```sh
case "$(uname -m)" in
  x86_64) architecture=amd64 ;;
  aarch64) architecture=arm64 ;;
  *) printf 'Native amd64 or arm64 host required\n' >&2; exit 1 ;;
esac
version=$(cat VERSION)
revision=$(git rev-parse HEAD)

podman --cgroup-manager=cgroupfs --events-backend=file build \
  --no-cache --format oci -f Containerfile \
  --build-arg VERSION="$version" --build-arg REVISION="$revision" \
  -t localhost/sonarr-remediator:dev .

mise exec -- python scripts/podman-smoke.py \
  --image localhost/sonarr-remediator:dev --architecture "$architecture" \
  --output artifacts/runtime.json
```

Always supply **both** build arguments. The binary version comes from the copied
`VERSION` file, but the image version label comes from the build argument; the
Containerfile's default version is stale and its revision default is `unknown`.
`--no-cache` avoids cached LABEL reuse observed with Podman 5.4.2. A dirty working
tree produces a local development image even if its label names `HEAD`; it is not
proof that the image was built from that commit alone.

The harness builds a fixture image and checks application UID/GID 65532, private
state, read-only root/config, absence of inbound ports/media/socket mounts,
dry-run behavior, local queries, health, persistence, bounded shutdown, failure
exit codes, and remediation behavior against the fixture. It writes sanitized
runtime evidence and removes its disposable containers/network/fixture image.
The application image remains in local storage.

To export the locally tested image without creating promotion metadata:

```sh
podman --cgroup-manager=cgroupfs --events-backend=file save \
  --format oci-archive -o artifacts/image.oci.tar \
  localhost/sonarr-remediator:dev
```

## Why `ci:podman` Is Not a Standalone Local Smoke Task

`mise run ci:podman` builds with explicit version/revision arguments, runs the
same smoke harness, exports the archive, and then creates GitHub-bound platform
metadata. It requires more than the two variables named in its task description:

- `SRA_ARCHITECTURE` must match the native host and `SRA_REVISION` must identify
  the tested checkout.
- A genuine `pull_request` workflow targeting `main` must supply
  `GITHUB_EVENT_PATH`, `GITHUB_EVENT_NAME`, `GITHUB_REPOSITORY`, `GITHUB_SHA`,
  `GITHUB_RUN_ID`, and `GITHUB_RUN_ATTEMPT`.
- The checkout must be that PR's merge ref, with both parents available and
  matching the event's base/head SHAs (CI uses checkout depth two).
- `GH_TOKEN` or `GITHUB_TOKEN` must permit the workflow's GitHub API reads.

The workflow already provides this context. Merely setting `SRA_ARCHITECTURE`
and `SRA_REVISION` locally is insufficient: metadata generation will fail after
the build/smoke/export stages. Do not fabricate workflow metadata to bypass the
contract. Use the local commands above; hosted CI supplies the promotable
archives and receipt for both architectures.
