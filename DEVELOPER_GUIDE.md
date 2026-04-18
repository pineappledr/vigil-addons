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
8. [Action Schema (smart-table)](#action-schema-smart-table)
9. [Migration from FormComponent](#migration-from-formcomponent)

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

---

## Action Schema (smart-table)

*Added in Phase 8 — promotes the `smart-table` manifest contract from "implicit zfs-manager behaviour" to the documented standard that `snapraid`, `burn-in`, and future add-ons write against.*

A `smart-table` component renders tabular data from a `source` endpoint, applies formatters, and attaches user actions. Actions live on three surfaces:

| Surface           | Rendered                           | Typical use |
|-------------------|------------------------------------|-------------|
| `toolbar_actions` | Buttons above the table            | "Create", "Start Sync", operations that don't target a specific row |
| `row_actions`     | Buttons in auto-injected last column | Per-row: "Delete", "Edit", "Rollback", "Cancel Job" |
| `bulk_actions`    | Buttons in a bar above the table when rows are selected | Batch delete, batch cancel, applied to every checked row |

Every action follows the same three-part shape:

```jsonc
{
  "id":           "unique-within-table",  // also used by invokeActionById()
  "label":        "Button Label",
  "icon":         "trash-2",               // optional; see icon registry
  "safety_tier":  "yellow",                // green | yellow | red | black
  "hidden":       false,                   // toolbar only; hide button but keep invokable

  "action":       { /* what the HTTP request looks like — required */ },
  "form":         { /* multi-field modal */ },       // OR
  "confirm":      { /* single dialog */ }            // pick ONE (or neither for immediate fire)
}
```

### `action` — the HTTP contract

```jsonc
"action": {
  "method":   "POST",                      // GET | POST | PUT | DELETE | PATCH
  "endpoint": "/api/pool/create",          // agent-relative; proxy appends ?agent_id
  "body":     { "force": false },          // static fields (shallow-merged under form values)
  "body_map": { "pool": "row.name" },      // row.X → body keys
  "preview":  {                            // optional: debounced CLI preview fired on input
    "endpoint": "/api/preview",
    "method":   "POST",
    "body":     { "action": "pool-create" }
  }
}
```

**Method tunnelling:** `PUT` and `DELETE` are tunneled through `POST /api/addons/{id}/proxy?path=…&method=DELETE` — the Vigil hub's proxy only accepts GET/POST directly. Smart-table handles this transparently.

**Path templating:** `endpoint` supports `{row.X}` and `{key_field}` substitution.

### `form` — multi-field modal

Use when the action needs several inputs (create, edit, multi-step workflows).

```jsonc
"form": {
  "title":           "Create Dataset — {row.name}",  // {row.X} interpolated
  "show_command":    true,                            // shows Command Preview panel if action.preview set
  "button_label":    "Create",
  "button_variant":  "primary",                       // primary | warning | danger
  "success_message": "Dataset {name} created",        // toast on 2xx; form values interpolate
  "error_message":   "Create failed: {error}",        // toast on non-2xx
  "wide":            true,                            // widens modal for multi-column forms
  "fields":          [ /* see Field types */ ]
}
```

### `confirm` — single dialog

Use when the user just needs to confirm, optionally with a type-to-confirm gate.

```jsonc
"confirm": {
  "title":                "Export {row.name}",
  "message":              "This unmounts every dataset on {row.name}.",
  "show_command":         true,
  "button_label":         "Export Pool",
  "button_variant":       "danger",
  "require_type_confirm": true,
  "confirm_value":        "{row.name}",           // what the user must type
  "confirm_key":          "confirm",               // body key for the typed value
  "confirm_label":        "Type the pool name",
  "extra_fields": [                                // inputs alongside the type-confirm
    { "key": "force", "type": "checkbox", "label": "Force export" }
  ],
  "success_message":      "Pool {row.name} exported",
  "error_message":        "Export failed: {error}"
}
```

### Field types

Every field has `key`, `type`, `label`. Dotted keys (`properties.compression`) collapse into nested body objects on submit: `properties.compression=lz4` becomes `{ "properties": { "compression": "lz4" } }`.

| `type`             | Extra props                                 | Behaviour |
|--------------------|---------------------------------------------|-----------|
| `text`             | `placeholder`, `hint`, `pattern`            | `<input type="text">` |
| `number`           | `min`, `max`, `step`, `default`             | `<input type="number">` |
| `select`           | `options` *or* `options_from`               | `<select>` |
| `multi-select`     | `options` *or* `options_from`, `size`       | `<select multiple>` |
| `checkbox`         | `default`                                   | `<input type="checkbox">` |
| `toggle`           | `default`                                   | iOS-style toggle |
| `hidden`           | `value`                                     | Invisible, but sent in body |
| `capacity_preview` | `type_field`, `devices_field`, `size_from`  | Live client-side ZFS capacity calculation |

Shared field properties:

| Prop             | Applies to               | Purpose |
|------------------|--------------------------|---------|
| `required`       | all                      | Blocks submit if empty |
| `value_from`     | all                      | Prefill from row — `"row.X"` |
| `visible_when`   | all                      | `{ field: "row.mode" \| "<form-field-key>", equals: … }` |
| `safety_tier`    | all                      | Red-highlights one destructive field in an otherwise-yellow form |
| `options`        | `select`, `multi-select` | Static — `[{ value, label, detail? }]` |
| `options_from`   | `select`, `multi-select` | Async fetch via addon proxy. Auto-appends `agent_id` |
| `option_value`   | with `options_from`      | JSON key → option.value (default `value`) |
| `option_label`   | with `options_from`      | JSON key → option label (default `label`) |
| `option_detail`  | with `options_from`      | JSON key → " — detail" suffix |

**Select value types.** `<select>.value` is always a string in the DOM, but smart-table preserves the JSON type you declared. An option written as `{ "value": 12, "label": "12 (4K)" }` is submitted as the number `12`; `{ "value": true, "label": "On" }` is submitted as boolean `true`. Strings pass through unchanged. The same coercion applies to `options_from` responses — if the fetched JSON value is a number or boolean, the submitted body preserves it. Without this, Go handlers typed `int` / `bool` would reject string input with `400 Bad Request` (`json: cannot unmarshal string into Go struct field …`).

### Column formatters

| `format`          | Input                        | Rendered as |
|-------------------|------------------------------|-------------|
| *(default)*       | any                          | Escaped text; arrays joined with `, ` |
| `bytes`           | number                       | `1.5 GB` |
| `percent`         | number                       | `12.3%` |
| `duration`        | seconds                      | `2m 15s` |
| `datetime`        | ISO 8601                     | Locale date/time |
| `relative_time`   | ISO 8601                     | `5 min ago` |
| `badge`           | string                       | Pill via `badge_map`: `success`, `warning`, `critical`, `muted`, `info` |
| `status_dot`      | `online`/`offline`/`busy`    | Coloured dot + label |
| `warning_badge`   | truthy                       | Red "OS" style badge |
| `row_actions`     | *(auto-injected)*            | The per-row button strip |

When `row_actions` is declared and no explicit `format: "actions"` column exists, smart-table appends a right-aligned actions column automatically.

### Data binding & templating

Interpolation is available in every string field of an action's `form` / `confirm` / `action`:

| Template      | Resolved from                           | Example |
|---------------|-----------------------------------------|---------|
| `{row.X}`     | The clicked row                         | `"Export {row.name}"` → `"Export tank"` |
| `{X}`         | Form values (in `success_message` etc.) | `"Dataset {name} created"` |
| `{key_field}` | Bulk-action per-row value               | `"/api/snapshots/{key_field}"` |
| `{count}`     | Bulk-action selection size              | `"Delete {count} snapshots?"` |
| `{error}`     | Server-returned `error` / `message`     | `"Export failed: {error}"` |

### Safety tiers

The tier controls both button colour and the implicit confirmation flow.

| Tier     | Button colour       | Default confirmation | Typical use |
|----------|---------------------|----------------------|-------------|
| `green`  | Success (green)     | None — fires on click (or simple modal if `form`/`confirm` present) | View, refresh, rescan |
| `yellow` | Warning (amber)     | `confirm` or `form` dialog | Create, edit, start scrub |
| `red`    | Danger (red)        | `confirm` + `require_type_confirm` recommended | Delete, rollback |
| `black`  | Solid red + bold outline | `confirm` + `require_type_confirm` mandatory | Pool create, pool destroy |

### Multi-agent routing

`smart-table` supports two routing modes for hub + per-host-agent add-ons:

**Page-level selection** — the user picks an agent from a dropdown; every action on the page routes to that agent. Enabled via `page_config.agent_selector: true`.

**Per-row routing** — when a table aggregates rows from multiple agents, include `agent_id` on each row. The framework uses that row's `agent_id` for its actions instead of the page selector. This is how the ZFS Manager's aggregated drive list dispatches per-disk actions to the correct host.

### Progress indicators

Long-running operations surface in the table via `progress_indicator`:

```jsonc
"progress_indicator": {
  "field": "scrub_status",
  "busy_values": ["in_progress", "resilvering", "scanning"],
  "interval_seconds": 5
}
```

When any row's `field` matches (exact or prefix — `"in_progress (35% done)"` counts), a pulsing dot appears next to the value and the table auto-refreshes on the interval until no row is busy.

### Bulk actions

Opt in with `selectable: true` and `key_field: "unique-field"` on the table config. A checkbox column appears, and `bulk_actions` buttons reveal in a bar when at least one row is selected.

```jsonc
"selectable":  true,
"key_field":   "full_name",
"bulk_actions": [
  {
    "id": "bulk-delete",
    "label": "Delete selected",
    "safety_tier": "red",
    "action": { "method": "DELETE", "endpoint": "/api/snapshots/{key_field}" },
    "confirm": {
      "title":                "Delete {count} snapshots?",
      "message":              "This cannot be undone. Deletes {count} snapshots across all checked rows.",
      "button_variant":       "danger",
      "require_type_confirm": true,
      "confirm_value":        "DELETE",
      "confirm_label":        "Type DELETE"
    }
  }
]
```

The framework iterates the selected rows, fires the action once per row, and aggregates successes/failures into a single summary toast. No agent-side batch endpoint is required, but if one exists you can hit it with one request by pointing `endpoint` at it and using `body_map: { "keys": "selection" }`.

### Icons

Resolved against an inline registry in `smart-table.js`. Available names include: `edit`, `plus`, `plus-square`, `trash`, `trash-2`, `refresh-cw`, `play`, `pause`, `x-circle`, `log-in`, `log-out`, `hard-drive`, `disc`, `settings`, `check`, `x`. Unknown names render as no-icon.

### Toasts & feedback

Every action with a `form` or `confirm` shape may declare `success_message` / `error_message`. These drive the toast rendered by `Utils.toast(message, type)` in vigil-core. Always set them for `yellow`/`red`/`black` actions — users on slow networks need confirmation beyond the modal closing.

### Validator allowlist (hub-side)

Vigil-core validates every registered manifest against a whitelist in `vigil/internal/addons/manifest.go` (`validComponentTypes` map). Unknown types fail registration with `Invalid manifest: unknown type 'X'`. Currently allowed: `smart-table`, `config-card`, `discovery-card`, `chart`, `progress`, `log-viewer`, `deploy-wizard`, `form`, `disk-storage`. File a PR against vigil-core to add new component types.

### Complete reference

```jsonc
{
  "type": "smart-table",
  "id":   "pool-list",
  "title":"ZFS Pools",
  "config": {
    "source": "/api/pools",
    "columns": [
      { "key": "name",   "label": "Pool",   "sortable": true },
      { "key": "health", "label": "Health", "format": "badge", "badge_map": {
          "ONLINE": "success", "DEGRADED": "warning", "FAULTED": "critical"
      }},
      { "key": "size",   "label": "Size",   "format": "bytes" }
    ],

    "sortable":      true,
    "filterable":    true,
    "default_sort":  { "key": "name", "direction": "asc" },
    "empty_message": "No pools on this host yet.",
    "selectable":    true,
    "key_field":     "name",
    "progress_indicator": {
      "field":            "scrub_status",
      "busy_values":      ["in_progress", "resilvering"],
      "interval_seconds": 5
    },

    "toolbar_actions": [
      {
        "id":           "create-pool",
        "label":        "Create Pool",
        "icon":         "plus-square",
        "safety_tier":  "yellow",
        "action":       { "method": "POST", "endpoint": "/api/pool/create",
                          "preview": { "endpoint": "/api/preview",
                                       "body": { "action": "pool-create" } } },
        "form": {
          "title":           "Create Pool",
          "show_command":    true,
          "button_label":    "Create",
          "button_variant":  "primary",
          "success_message": "Pool {name} created",
          "error_message":   "Create failed: {error}",
          "fields": [
            { "key": "name",       "type": "text", "label": "Pool Name", "required": true },
            { "key": "data_type",  "type": "select", "label": "Layout",
              "options": [
                { "value": "mirror", "label": "Mirror" },
                { "value": "raidz1", "label": "RAIDZ1" },
                { "value": "raidz2", "label": "RAIDZ2" }
              ] },
            { "key": "data_devices", "type": "multi-select", "label": "Data Devices",
              "required": true, "options_from": "/api/disks?unused=true",
              "option_value": "path", "option_label": "model", "option_detail": "serial" }
          ]
        }
      }
    ],

    "row_actions": [
      {
        "id":           "export-pool",
        "label":        "Export",
        "icon":         "log-out",
        "safety_tier":  "red",
        "action":       { "method": "POST", "endpoint": "/api/pool/export",
                          "body_map": { "name": "row.name" } },
        "confirm": {
          "title":                "Export {row.name}",
          "message":              "This unmounts every dataset on {row.name}.",
          "button_variant":       "danger",
          "require_type_confirm": true,
          "confirm_value":        "{row.name}",
          "confirm_label":        "Type the pool name",
          "extra_fields": [
            { "key": "force", "type": "checkbox", "label": "Force export" }
          ],
          "success_message":      "Pool {row.name} exported",
          "error_message":        "Export failed: {error}"
        }
      }
    ],

    "bulk_actions": [
      {
        "id":          "bulk-export",
        "label":       "Export selected",
        "safety_tier": "red",
        "action":      { "method": "POST", "endpoint": "/api/pool/export",
                         "body_map": { "name": "row.name" } },
        "confirm": {
          "title":           "Export {count} pools?",
          "message":         "This unmounts datasets on every selected pool.",
          "button_variant":  "danger"
        }
      }
    ]
  }
}
```

---

## Migration from FormComponent

`vigil/web/js/components/form.js` is the pre-Phase-7 form renderer. It predates the action schema above and has hardcoded behaviours that do not compose with the smart-table pipeline:

- Only three `source` values are supported (`addon_agents`, `agent_drives`, `job_history`) — arbitrary `options_from` is not.
- The `security_gate` field emits a multi-step modal flow (password → path confirm) that `require_type_confirm` superseded.
- `depends_on` cascading selects work only for those hardcoded sources.
- Action dispatch goes to a fixed `action: "config"` / `action: "execute"` endpoint pair on the hub — not an arbitrary endpoint from the manifest.

**Decision (Phase 8.3):** Keep `FormComponent` in vigil-core for backward compatibility — the `snapraid` Automation page and `burn-in` New Job page still depend on it. **Do not use it in new manifests.** For any new workflow that needs more than a trivial "config update" form, declare a `toolbar_action` on a relevant `smart-table` with the full `form` shape.

When migrating an existing `type: "form"` page to a toolbar action:

| `FormComponent`                    | Smart-table equivalent |
|------------------------------------|------------------------|
| `source: "addon_agents"`           | `options_from: "/api/agents"` with `option_value: "agent_id"`, `option_label: "hostname"` |
| `source: "agent_drives"`           | `options_from: "/api/disks"` (or `/api/agent/drives`) |
| `security_gate: true` on checkbox  | `require_type_confirm: true` + `confirm_value: "DESTROY"` on the `confirm` block |
| `depends_on: "agent_id"`           | Not yet supported on smart-table forms — tracked as future work |
| `visible_when: { field: [v1, v2] }` | `visible_when: { field: "X", equals: v1 }` (single value) |

### Deprecation target

Once every add-on has migrated, delete `form.js` and remove `"form"` from the vigil-core `validComponentTypes` allowlist. Migration status:

- [ ] `snapraid` — **Automation** (`automation_form`)
- [ ] `snapraid` — **Operations** (`command_form`, 5 sub-actions)
- [ ] `burn-in` — **New Job** (`job-form`, 9 fields + security gate)

Phase 8.2 migrated the easy wins (agent deletion, job cancellation) to `row_actions`. The three pages above are larger refactors — deferred because their current forms work, and the cascading-select feature they rely on (`depends_on`) is not yet in the smart-table contract.

---

## Changelog

| Date       | Change |
|------------|--------|
| 2026-04-18 | Added "Action Schema (smart-table)" and "Migration from FormComponent" sections (Phase 8). |
