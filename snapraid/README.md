# SnapRAID Add-on for Vigil

Full lifecycle management of local SnapRAID arrays through the Vigil ecosystem. Replaces external automation scripts (`snapraid-runner`, `snapraid-aio-script`, cron wrappers) with a native Go scheduling engine, safety gates, and a manifest-driven UI.

## Architecture

The add-on uses a **two-tier Hub/Agent model** consistent with all Vigil add-ons:

```
Vigil Server (:9080)
    │  POST /api/addons/connect   (registration + manifest)
    │  WS   /api/addons/ws        (telemetry upstream)
    │
SnapRAID Hub (:9300)
    │  REST + WebSocket
    │
SnapRAID Agent (:9400)   ← one per NAS host
    │
    └─ snapraid binary
```

| Component | Responsibility |
|-----------|---------------|
| **Hub** | Agent registry, command routing, telemetry aggregation, upstream Vigil Server connection |
| **Agent** | SnapRAID CLI execution, native scheduling, safety gates, job history, real-time log streaming |

The Hub is lightweight and stateless aside from a JSON-persisted Agent registry. All heavy state (job history, config cache, telemetry queue) lives on the Agent in a local SQLite database.

### Vigil Server Connection

The Hub connects to the Vigil Server using the same two-phase pattern as all Vigil add-ons:

1. **Registration** — On startup, the Hub sends `POST /api/addons/connect` with its embedded manifest and the one-time registration token (generated in the Vigil UI "Add Add-on" dialog). The server responds with an `addon_id`.
2. **Telemetry** — The Hub opens a persistent WebSocket to `/api/addons/ws?addon_id=N` and forwards aggregated Agent telemetry upstream. Heartbeats are sent every 30 seconds. The connection auto-reconnects with exponential backoff on failure.

If no `vigil.token` is configured, the Hub runs in standalone mode without upstream connectivity.

## Deployment

### Docker Compose (Recommended)

Create a `docker-compose.yml`:

A `docker-compose.yml` is included in the repository. Copy it alongside your config files:

```yaml
services:
  snapraid-hub:
    container_name: Snapraid-Hub
    image: ghcr.io/pineappledr/vigil-addons-snapraid-hub:latest
    restart: unless-stopped
    ports:
      - "9300:9300"
    command: ["-config", "/etc/snapraid-hub/config.hub.yaml"]
    volumes:
      - hub-data:/data
      - ./config.hub.yaml:/etc/snapraid-hub/config.hub.yaml:ro

  snapraid-agent:
    container_name: Snapraid-Agent
    image: ghcr.io/pineappledr/vigil-addons-snapraid-agent:latest
    restart: unless-stopped
    privileged: true
    ports:
      - "9400:9400"
    command: ["-config", "/etc/snapraid-agent/config.agent.yaml"]
    volumes:
      - agent-data:/var/lib/vigil-snapraid-agent
      - ./config.agent.yaml:/etc/snapraid-agent/config.agent.yaml:ro
      - /etc/snapraid.conf:/etc/snapraid.conf:ro
      # Mount all data and parity disks used by snapraid:
      # - /mnt/data1:/mnt/data1
      # - /mnt/data2:/mnt/data2
      # - /mnt/parity:/mnt/parity

volumes:
  hub-data:
  agent-data:
```

Uncomment and adjust the disk mount lines for your array layout.

Pull and start both services:

```bash
docker compose pull
docker compose up -d
```

### Standalone Binaries

Retrieve the binaries from the GitHub Releases page for your architecture:

```bash
# Hub
chmod +x snapraid-hub-linux-amd64
./snapraid-hub-linux-amd64 -config config.hub.yaml

# Agent
chmod +x snapraid-agent-linux-amd64
./snapraid-agent-linux-amd64 -config config.agent.yaml -db /var/lib/vigil-snapraid-agent/agent.db
```

## Configuration

Copy the example files and adjust for your environment:

```bash
cp config.hub.example.yaml config.hub.yaml
cp config.agent.example.yaml config.agent.yaml
```

See the example files for comprehensive documentation of every option.

### Environment Variable Overrides

All configuration values can be overridden via environment variables:

| Prefix | Binary |
|--------|--------|
| `VIGIL_SNAPRAID_HUB_` | Hub |
| `VIGIL_SNAPRAID_AGENT_` | Agent |

Variable names use uppercase with underscores matching the YAML path:

```bash
VIGIL_SNAPRAID_AGENT_LISTEN_PORT=9400
VIGIL_SNAPRAID_AGENT_SCHEDULER_MAINTENANCE_CRON="0 3 * * *"
VIGIL_SNAPRAID_AGENT_THRESHOLDS_MAX_DELETED=100
```

### Precedence (Agent)

1. `POST /api/config` from Hub (highest, persisted in SQLite)
2. Environment variables
3. YAML configuration file
4. Built-in defaults

## Multi-Host Setup

For multiple NAS hosts, deploy **one Hub** and **one Agent per host**:

1. Deploy the Hub on any reachable host (or alongside the Vigil Server).
2. On each NAS host, deploy an Agent with its own `config.agent.yaml` pointing `hub.url` to the Hub's address.
3. Each Agent self-registers with the Hub on startup.
4. The Vigil UI Dashboard includes an Agent selector dropdown to switch between hosts.

All Agents share the same Hub. Each Agent manages its own local SnapRAID array independently.

## Scheduling & Safety

The Agent runs a native cron scheduler with four job types:

| Job | Default | Pipeline |
|-----|---------|----------|
| **Maintenance** | Daily 03:00 | `touch` → `diff` → gates → `sync` → `scrub` |
| **Scrub** | Sunday 04:00 | Standalone `scrub` |
| **SMART** | Every 6h | `smart` check with failure detection |
| **Status** | Every 30m | `status` refresh for dashboard telemetry |

### Pre-Flight Safety Gates

Before any automated sync, three gates must pass:

1. **SMART Gate** — Aborts if any disk reports `FAIL`/`PREFAIL` or exceeds the failure probability threshold.
2. **Diff Threshold Gate** — Aborts if deleted or updated files exceed configured limits. The `add_del_ratio` can override a deletion breach.
3. **Concurrency Lock** — Ensures no other SnapRAID operation is running.

## API Reference

### Agent Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check |
| `POST` | `/api/execute` | Trigger a SnapRAID command (sync, scrub, fix, status, smart, diff, touch) |
| `POST` | `/api/config` | Push configuration updates (persisted to SQLite) |
| `GET` | `/api/jobs` | Retrieve recent job history |

### Hub Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check |
| `GET` | `/api/deploy-info` | Returns Hub URL and token for the deploy-wizard prefill |
| `POST` | `/api/agents/register` | Agent self-registration |
| `GET` | `/api/agents` | List registered Agents |
| `POST` | `/api/command` | Route a command to a target Agent |
| `POST` | `/api/telemetry/ingest` | Receive Agent telemetry |
| `POST` | `/api/config/{agentID}` | Forward config update to Agent |

## Troubleshooting

### Hub cannot connect to Vigil Server

The Hub retries registration with exponential backoff (2s to 60s). Check:

1. `vigil.server_url` in `config.hub.yaml` points to the correct Vigil Server HTTP address (e.g., `http://192.168.1.10:9080`).
2. `vigil.token` matches a token generated in the Vigil UI "Add Add-on" dialog. Tokens expire after 1 hour.
3. The token must be bound to an add-on in the Vigil UI before the Hub can connect. If you see `"Token not yet bound"` errors, complete the registration form in the Vigil UI first.
4. If the Hub starts without a token (`vigil.token: ""`), it runs in standalone mode and logs a warning.

### Agent fails to start: "invalid cron expression"

The Agent validates all cron expressions on startup. Ensure you use standard 5-field format:

```
┌───────────── minute (0-59)
│ ┌───────────── hour (0-23)
│ │ ┌───────────── day of month (1-31)
│ │ │ ┌───────────── month (1-12)
│ │ │ │ ┌───────────── day of week (0-6, Sun=0)
│ │ │ │ │
* * * * *
```

### Agent returns 409 Conflict on execute

Only one SnapRAID operation can run at a time. The engine uses a mutex to enforce this. Wait for the current operation to finish, or use the Abort button on the Operations page.

### Hub cannot reach Agent

Verify the Agent's `listen.port` is accessible from the Hub host. In Docker, ensure both containers share a network or use `host.docker.internal`. Check firewall rules for the Agent port (default 9400).

### Job history growing too large

The Agent automatically prunes job records older than 90 days. This runs once daily on startup and every 24 hours thereafter. No manual intervention is needed.

### Database permission errors

The Agent creates its SQLite database with restricted permissions (0600). Ensure the process user has write access to the database directory (`/var/lib/vigil-snapraid-agent/` by default).

## Development

```bash
# Build both binaries
go build ./cmd/hub
go build ./cmd/agent

# Run tests
go test -race ./...

# Vet
go vet ./...
```

## License

See the repository root for license information.
