# Sonarr Recovery Agent

A Go microservice that runs alongside Sonarr as a sidecar container, autonomously detecting, analyzing, and recovering from common download and import issues.

## Quick Start

1. Copy `config.example.yaml` to `config.yaml` and configure:
   - `sonarr.url` — your Sonarr instance URL
   - `sonarr.apiKey` — your Sonarr API key

2. Run in dry-run mode (default):
   ```
   go run ./cmd/sonarr-remediator --config config.yaml
   ```

3. Once satisfied with behavior, set `dryRun: false` and restart.

## Features

- **Queue Monitoring** — Continuously polls Sonarr's download queue
- **Stuck Download Cleanup** — Removes downloads that will never import
- **Not Custom Format Upgrade Removal** — Cleans up non-upgrade downloads
- **Import Recovery** — Automatically recovers failed imports with confidence scoring
- **Manual Import Assistant** — Dashboard-based review for medium-confidence matches
- **Retry Scheduling** — Retries transient import failures on configurable intervals
- **Intelligent Cleanup** — Removes empty folders, samples, NFOs, broken symlinks
- **Web Dashboard** — Real-time visibility and manual approvals
- **Notifications** — Discord, Slack, Gotify, ntfy, webhooks, email
- **Prometheus Metrics** — Full observability
- **Dry Run Mode** — Observe safely before enabling automation

## Configuration

All settings in `config.yaml` with `SRA_` environment variable overrides.