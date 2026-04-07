# Vigil Add-on Developer Guide

This guide explains how to build, package, and publish a new add-on for the [Vigil](https://github.com/pineappledr/vigil) monitoring platform.

---

## Table of Contents

1. [Overview](#overview)
2. [Hub/Agent Pattern](#hubagent-pattern)
3. [JSON UI Manifest](#json-ui-manifest)
4. [WebSocket Communication Protocol](#websocket-communication-protocol)
5. [Multi-Architecture Builds](#multi-architecture-builds)
6. [CI/CD Pipeline Conventions](#cicd-pipeline-conventions)
7. [Development Workflow](#development-workflow)

---

## Overview

A Vigil add-on is a standalone Go daemon (or any language that can speak WebSocket + JSON) that:

1. **Registers** with the Vigil server by posting its JSON UI manifest to `POST /api/addons/connect` with a Bearer token.
2. **Connects** via a persistent WebSocket at `ws://<vigil-server>/api/addons/ws?addon_id=<ID>`.
3. **Streams** telemetry frames (progress, logs, notifications, heartbeats) to the Vigil server.
4. **Renders** its UI dynamically in the Vigil dashboard via the manifest -- no frontend code required.

### Registration Flow

Registration is a two-phase process:

1. **Admin UI** -- In the Vigil dashboard, an admin clicks "Add Add-on", fills in the name and URL, and generates a one-time registration token (expires in 1 hour). This creates a placeholder add-on record and binds the token to it via `POST /api/addons/register`.

2. **Add-on Self-Registration** -- The add-on daemon starts and calls `POST /api/addons/connect` with the token in the `Authorization: Bearer <token>` header and its manifest in the request body. The server validates the token, stores the manifest, and returns the `addon_id`.

After registration, the add-on opens a WebSocket connection for telemetry. The token is reused for WebSocket authentication on reconnect.

If the token is not yet bound to an add-on (admin hasn't completed step 1), the server returns `412 Precondition Failed` and the add-on should retry with backoff.

---

## Hub/Agent Pattern

Most add-ons in this repo use a **two-tier Hub/Agent model**. The Hub runs alongside the Vigil server (or anywhere on the network). Agents run on the hosts where the actual work happens. The Hub registers with Vigil, aggregates agent telemetry, and serves UI data. Agents register with the Hub and push telemetry.

```text
Vigil Server
    │  POST /api/addons/connect
    │  WS   /api/addons/ws
    │
Hub (port varies per addon)
    │  POST /api/agents/register    ← PSK required
    │  POST /api/telemetry/ingest   ← PSK required
    │  GET  /api/deploy-info        ← returns hub_url + hub_psk
    │
Agent (port varies per addon)   ← one per host
    │
    └─ host tooling (CLI, binaries, etc.)
```

### PSK Authentication

Because agents often execute privileged or destructive operations, every agent-to-hub request must include a **Pre-Shared Key**:

```text
Authorization: Bearer <psk>
```

**How the PSK lifecycle works:**

1. The Hub generates a cryptographically random 32-byte hex PSK on first boot and saves it to `/data/hub.psk` with `0600` permissions.
2. `GET /api/deploy-info` returns `{"hub_url": "...", "hub_psk": "..."}`. The `deploy-wizard` manifest component calls this endpoint automatically to pre-fill the generated Docker Compose.
3. Agents include the PSK in every request. The Hub rejects any missing or mismatched PSK with `401 Unauthorized`.
4. `POST /api/rotate-psk` (body: `{"confirm":"ROTATE"}`) generates a new PSK, persists it, and returns it. All agents must be redeployed with the new value.

**Implementation reference:** `shared/` libraries handle the registration and telemetry client. See `snapraid/internal/hub/` or `zfs-manager/internal/manager/` for the full server-side pattern.

### Shared Libraries

The `shared/` directory contains reusable packages available to all add-ons via `replace` directives in `go.mod`:

| Package              | Description                                                                      |
| -------------------- | -------------------------------------------------------------------------------- |
| `shared/addonutil`   | `WriteJSON(w, status, v)` and other HTTP helpers                                 |
| `shared/vigilclient` | Vigil registration (`POST /api/addons/connect`) and WebSocket telemetry client   |

```go
// go.mod
replace (
    github.com/pineappledr/vigil-addons/shared/addonutil  => ../shared/addonutil
    github.com/pineappledr/vigil-addons/shared/vigilclient => ../shared/vigilclient
)
```

### Agent Registry

The Hub persists registered agents to a JSON file (`/data/agents.json` by default). An agent is considered **online** if its `last_seen_at` is within 2 minutes. Agents update `last_seen_at` on every telemetry ingest (`POST /api/telemetry/ingest`).

### Telemetry Cache

The Hub caches the latest telemetry payload per agent in memory. UI data endpoints (`GET /api/pools`, `GET /api/datasets`, etc.) read from this cache. If the cache is empty for an agent, endpoints return `[]`.

---

## JSON UI Manifest

Every add-on must provide a JSON manifest that declares its name, version, and UI layout. The Vigil dashboard renders this manifest dynamically.

### Structure

```json
{
  "name": "my-addon",
  "version": "1.0.0",
  "description": "What this add-on does",
  "pages": [
    {
      "id": "dashboard",
      "title": "Dashboard",
      "icon": "activity",
      "components": [
        {
          "type": "progress",
          "id": "job-progress",
          "title": "Active Jobs",
          "config": { ... }
        }
      ]
    }
  ]
}
```

### Top-Level Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique identifier for the add-on |
| `version` | string | yes | Semantic version |
| `description` | string | yes | Brief description |
| `pages` | array | yes | UI pages to render in the dashboard |

### Page Object

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | string | yes | Unique page identifier |
| `title` | string | yes | Tab label in the dashboard |
| `icon` | string | no | Icon name (Lucide icon set) |
| `components` | array | yes | Components rendered on this page |

### Supported Component Types

| Type | Purpose | Key Config Options |
|------|---------|-------------------|
| `form` | Input forms with validation, field dependencies, and security gates | `fields[]`, `action`, `submit_label` |
| `progress` | Job progress bars with phase tracking, ETA, and throughput | `multi`, `show_phase`, `show_eta`, `show_speed`, `show_temperature` |
| `chart` | Chart.js-based data visualization (line, bar, etc.) | `chart_type`, `x_axis`, `y_axis`, `thresholds[]`, `max_points` |
| `smart-table` | Data tables with sorting, filtering, and expandable rows | `columns[]`, `source`, `sortable`, `filterable`, `expandable` |
| `disk-storage` | Visual disk storage cards with progress bars, color-coded usage, and inline alias editing | `source`, `aliases`, `thresholds` |
| `log-viewer` | Real-time streaming log viewer with level filtering | `max_lines`, `levels[]`, `show_timestamp`, `filterable_by_job` |
| `deploy-wizard` | Docker/binary deployment generator with prefill support | `docker`, `binary`, `target_label`, `prefill_endpoint` |

### Form Field Types

| Type | Description |
|------|-------------|
| `select` | Dropdown. Use `source` for dynamic options (`addon_agents`, `agent_drives`) |
| `number` | Numeric input with optional `min`/`max` |
| `text` | Free-text input |
| `toggle` | Boolean toggle switch |
| `checkbox` | Checkbox. Set `security_gate: true` for destructive confirmation |

### Conditional Visibility

Fields can be shown or hidden based on other field values using `depends_on` (for cascading dropdowns) or `visible_when` (for conditional sections):

```json
{
  "name": "block_size",
  "label": "Block Size",
  "type": "number",
  "visible_when": { "command": ["burnin", "full"] }
}
```

### Deploy Wizard Component

The `deploy-wizard` component generates Docker Compose or binary install instructions for deploying sub-components (e.g., agents). The Vigil UI renders this as a guided setup dialog with a "Copy docker compose" button.

```json
{
  "type": "deploy-wizard",
  "id": "add-agent",
  "title": "Add Agent",
  "config": {
    "target_label": "Agent",
    "docker": {
      "image": "ghcr.io/pineappledr/vigil-addons-my-agent",
      "default_tag": "latest",
      "container_name": "my-agent",
      "privileged": false,
      "ports": ["9200:9200"],
      "volumes": ["agent-data:/var/lib/agent"],
      "named_volumes": ["agent-data"],
      "environment": {
        "HUB_URL": { "source": "prefill", "key": "hub_url", "label": "Hub URL" },
        "HUB_PSK": { "source": "prefill", "key": "hub_psk", "label": "Pre-Shared Key" },
        "AGENT_ID": { "source": "user_input", "label": "Agent ID", "placeholder": "agent-01" },
        "TZ": { "source": "literal", "value": "${TZ:-UTC}" }
      },
      "platforms": {
        "linux": { "label": "Standard Linux", "hint": "Deploy on any Linux host." }
      }
    },
    "prefill_endpoint": "/api/deploy-info"
  }
}
```

**Environment variable sources:**

| Source | Description |
|--------|-------------|
| `prefill` | Auto-filled from the `prefill_endpoint` response. The `key` maps to a JSON field in the response. |
| `user_input` | User types a value in the UI. Shows `label` and `placeholder`. |
| `literal` | Hard-coded value, not editable. Supports shell variable expansion syntax. |

The `prefill_endpoint` is a relative path proxied through the Vigil Server to the add-on's own API (e.g., `GET /api/deploy-info`). The add-on should return a JSON object with keys matching the `prefill` environment variable `key` fields.

### Limits

| Constraint | Limit |
|------------|-------|
| Maximum pages | 20 |
| Maximum total components | 50 |
| Maximum form fields per form | 100 |
| Maximum manifest size | 256 KiB |

---

## WebSocket Communication Protocol

### Connection

Connect to the Vigil server at:

```
ws://<vigil-server>/api/addons/ws?addon_id=<ID>
```

Include the Bearer token in the WebSocket handshake:

```
Authorization: Bearer <agent_token>
```

### Frame Envelope

All frames use the same envelope structure:

```json
{
  "type": "<frame_type>",
  "payload": { ... }
}
```

### Frame Types

#### Heartbeat

Sent at a regular interval (default: 30 seconds) to keep the connection alive.

```json
{
  "type": "heartbeat",
  "payload": null
}
```

**Requirements:**
- Send at least every 60 seconds. The server expects activity within 90 seconds before marking the connection as dead.
- After 3 missed intervals, the add-on status transitions to `degraded` and an `addon_degraded` event is emitted.
- The server sends WebSocket pings every 30 seconds; respond to pongs automatically (most WebSocket libraries handle this).

#### Progress

Reports job execution progress. The Vigil dashboard renders this as a live progress bar.

```json
{
  "type": "progress",
  "payload": {
    "agent_id": "agent-01",
    "job_id": "burnin-sda-20260228T143000",
    "command": "burnin",
    "phase": "BADBLOCKS",
    "phase_detail": "pattern 2/4",
    "percent": 37.5,
    "speed_mbps": 185.2,
    "temp_c": 38,
    "elapsed_sec": 3600,
    "eta_sec": 5400,
    "badblocks_errors": 0,
    "smart_deltas": null
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `agent_id` | string | Identifier of the agent running the job |
| `job_id` | string | Unique job identifier |
| `command` | string | Job type (`burnin`, `preclear`, `full`) |
| `phase` | string | Current pipeline phase |
| `phase_detail` | string | Human-readable sub-phase detail |
| `percent` | float | Overall completion (0.0 - 100.0) |
| `speed_mbps` | float | Current throughput in MB/s |
| `temp_c` | int | Drive temperature in Celsius |
| `elapsed_sec` | int | Seconds since job start |
| `eta_sec` | int | Estimated seconds remaining |
| `badblocks_errors` | int | Cumulative bad block count |
| `smart_deltas` | object | SMART attribute delta map (attribute ID to change) |

#### Log

Streams log messages to the Vigil dashboard log viewer.

```json
{
  "type": "log",
  "payload": {
    "agent_id": "agent-01",
    "job_id": "burnin-sda-20260228T143000",
    "severity": "info",
    "message": "SMART short test completed without error",
    "timestamp": "2026-02-28T14:35:00Z"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `severity` | string | `info`, `warning`, or `error` |
| `message` | string | Log message text |
| `timestamp` | string | RFC 3339 timestamp |

#### Notification

Structured events that trigger alert dispatch through Vigil's notification channels (email, Discord, Telegram, etc.).

```json
{
  "type": "notification",
  "payload": {
    "event_type": "burnin_passed",
    "severity": "info",
    "source": "burnin-hub",
    "message": "Burn-in passed for WD Red 8TB (serial WD-ABC123)",
    "timestamp": "2026-02-28T18:00:00Z"
  }
}
```

### Event Types

| Event Type | Severity | Description |
|------------|----------|-------------|
| `job_started` | info | A new job has begun |
| `phase_complete` | info | A pipeline phase finished successfully |
| `burnin_passed` | info | Full burn-in cycle completed with no errors |
| `job_complete` | info | Job finished successfully |
| `job_failed` | critical | Job encountered an unrecoverable error |
| `smart_warning` | warning | SMART attribute degradation detected |
| `temp_alert` | warning/critical | Drive temperature exceeded threshold |
| `addon_degraded` | warning | Server-generated: add-on missed heartbeats |
| `addon_online` | info | Server-generated: add-on recovered from degraded |

### Protocol Constraints

| Constraint | Value |
|------------|-------|
| Maximum frame size | 64 KB |
| Heartbeat interval | Configurable, default 30s |
| Heartbeat timeout | 90s (server-side) |
| Reconnect backoff | Exponential: 2s base, 2x factor, 60s cap |
| Write deadline | 10s per frame |
| Pong timeout | 60s |

---

## Multi-Architecture Builds

All official add-ons must produce binaries and container images for both `linux/amd64` and `linux/arm64`.

### Dockerfile Template

Use a multi-stage build with platform arguments:

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.version=$VERSION" \
    -o /addon ./cmd/addon

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /addon /usr/local/bin/addon
ENTRYPOINT ["addon"]
```

### Build Command

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t ghcr.io/pineappledr/vigil-my-addon:latest \
  --push .
```

### Binary Cross-Compilation

```bash
VERSION="1.0.0"
LDFLAGS="-s -w -X main.version=$VERSION"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o dist/addon-linux-amd64 ./cmd/addon
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o dist/addon-linux-arm64 ./cmd/addon
```

---

## CI/CD Pipeline Conventions

Each add-on in the monorepo has its own GitHub Actions workflow with path filtering. The pipeline follows five stages:

| Stage | Trigger | Actions |
|-------|---------|---------|
| **Test & Lint** | All pushes and PRs | `go vet`, `go test -race`, coverage report, build check |
| **Security Scan** | All pushes and PRs | `govulncheck`, `gosec`, Trivy container scanning |
| **PR Preview** | Pull requests only | Build and publish preview images tagged `pr-<number>` |
| **Dev Build** | Feature branch push | Cross-compile binaries, publish dev release, push dev-tagged images |
| **Latest Build** | Push to `main` | Multi-arch build, push `latest`-tagged images |
| **Release** | Version tag (`v*`) | Multi-arch build, push versioned images, create GitHub Release with binaries and checksums |

### Path Filtering

Workflows only trigger when files within the add-on's directory change:

```yaml
# burn-in add-on (uses v* tags)
on:
  push:
    paths:
      - 'burn-in/**'
      - '.github/workflows/burnin.yml'
    tags: ['v*']

# snapraid add-on (uses snapraid-v* tags to avoid conflicts)
on:
  push:
    paths:
      - 'snapraid/**'
      - '.github/workflows/snapraid.yml'
    tags: ['snapraid-v*']
```

### Container Registry

All images are published to GitHub Container Registry (GHCR):

```
ghcr.io/pineappledr/vigil-addons-<addon>-<component>:<tag>
```

---

## Development Workflow

1. **Clone** the repository:
   ```bash
   git clone https://github.com/pineappledr/vigil-addons.git
   cd vigil-addons
   ```

2. **Create** your add-on directory:
   ```bash
   mkdir my-addon
   cd my-addon
   go mod init github.com/pineapple/vigil-addons/my-addon
   ```

3. **Implement** the core components:
   - `cmd/hub/main.go` -- Hub entry point with manifest embedding (`//go:embed manifest.json`)
   - `cmd/hub/manifest.json` -- UI manifest co-located for embedding
   - `internal/hub/vigil_client.go` -- Registration client (`POST /api/addons/connect`)
   - `internal/hub/vigil_telemetry.go` -- WebSocket telemetry client
   - Your domain-specific logic (agents, schedulers, etc.)

   For multi-tier add-ons (Hub/Agent pattern), also implement:
   - `cmd/agent/main.go` -- Agent entry point
   - Agent-to-Hub registration and telemetry push
   - Hub-side agent registry, command routing, and telemetry aggregation

4. **Create** Dockerfiles for each component (e.g., `Dockerfile.hub`, `Dockerfile.agent`).

5. **Add** a GitHub Actions workflow at `.github/workflows/my-addon.yml` following the conventions above. Use addon-specific tag prefixes (e.g., `myaddon-v*`) to avoid conflicts with other add-ons in the monorepo.

6. **Test** locally against a running Vigil server:
   - In the Vigil dashboard, generate a registration token and fill in the "Add Add-on" form
   - Configure your Hub with the `server_url` and `token` from the UI
   - Start the Hub -- it will call `POST /api/addons/connect` to register and open a WebSocket for telemetry
   - Verify the manifest renders correctly and telemetry frames appear in real-time

7. **Open** a pull request with your add-on, tests, and documentation.
