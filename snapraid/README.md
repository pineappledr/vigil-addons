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

## Prerequisites

### Agent Host Requirements

Each host running a SnapRAID Agent must have:

1. **SnapRAID installed** — The Agent wraps the `snapraid` binary. Install it via your package manager or from [snapraid.it](https://www.snapraid.it):
   ```bash
   # Debian/Ubuntu
   apt install snapraid

   # Arch
   pacman -S snapraid

   # From source
   wget https://github.com/amadvance/snapraid/releases/download/v12.3/snapraid-12.3.tar.gz
   tar xzf snapraid-12.3.tar.gz && cd snapraid-12.3
   ./configure && make && sudo make install
   ```

2. **A valid `snapraid.conf`** — The Agent reads this file to discover content and parity file paths. It must be configured and working before deploying the Agent. Test with `snapraid status` first.

3. **Data and parity disks mounted** — All disks referenced in `snapraid.conf` must be mounted and accessible. When running in Docker, these must be bind-mounted into the Agent container.

4. **Docker (optional)** — Only required if using Docker container pause/stop features or deploying via docker-compose. The Agent itself can run as a standalone binary.

### Hub Requirements

The Hub has no special requirements beyond network connectivity. It can run on any host that can reach both the Vigil Server and the Agent(s).

### Network Ports

| Port | Service | Direction |
|------|---------|-----------|
| 9080 | Vigil Server | Hub → Server |
| 9300 | SnapRAID Hub | Agent → Hub, Server → Hub |
| 9400 | SnapRAID Agent | Hub → Agent |

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

## Dashboard Pages

The Vigil UI renders five pages from the Hub manifest:

| Page | Components | Purpose |
|------|-----------|---------|
| **Dashboard** | Disk Status table, Active Job progress, SMART Overview | At-a-glance array health and running operations |
| **Operations** | Execute Command form | Manually trigger sync, scrub, fix, status, smart, diff, or touch against a selected Agent |
| **Automation** | Schedule Configuration form | Configure maintenance, scrub, and SMART schedules with presets or custom cron; set safety thresholds |
| **Agents** | Registered Agents table, Deploy Wizard | View connected Agents and deploy new ones via generated docker-compose |
| **Logs** | Live Output viewer, Job History table | Real-time log streaming and historical job records |

### Automation Settings

All schedule and threshold settings are configured per-Agent from the Automation page:

| Setting | Type | Default | Description |
|---------|------|---------|-------------|
| **Maintenance Schedule** | Preset / Custom cron | Daily at 3:00 AM | Runs `touch` → `diff` → safety gates → `sync` → `scrub` |
| **Scrub Schedule** | Preset / Custom cron | Sundays at 4:00 AM | Standalone scrub to verify data integrity |
| **SMART Check Schedule** | Preset / Custom cron | Every 6 hours | Polls drive health and reports failures |
| **Deletion Threshold** | Number | 50 | Abort sync if more files deleted than this limit (0 = no limit) |
| **Update Threshold** | Number | -1 | Abort sync if more files updated than this limit (-1 = disabled) |
| **Enable Pre-Hash** | Toggle | Off | Hash files before sync to detect silent corruption (slower but safer) |
| **Default Scrub Plan** | Select | 8% | How much data to verify per scrub run (bad, new, full, or percentage) |
| **Scrub Min Age (days)** | Number | 10 | Only scrub blocks older than this many days since last check |
| **Auto-Fix Bad Blocks** | Toggle | Off | Automatically attempt to repair bad blocks detected during scrub |
| **Pre-Sync Hook** | Text | — | Shell command to run before each sync (e.g. stop services, flush caches) |
| **Post-Sync Hook** | Text | — | Shell command to run after each sync (e.g. restart services, send reports) |
| **Pause Containers Before Sync** | Text | — | Comma-separated Docker container names to pause during sync and unpause after |
| **Stop Containers Before Sync** | Text | — | Comma-separated Docker container names to stop during sync and restart after |

Schedule fields offer common presets (e.g., "Daily at 3:00 AM", "Sundays at 4:00 AM", "Every 6 hours") plus a **Custom** option that reveals a text input for standard 5-field cron expressions.

### Recommended Configurations

**Home NAS (1-8 drives, light usage)**

| Setting | Value | Why |
|---------|-------|-----|
| Maintenance Schedule | Daily at 3:00 AM | Nightly sync keeps parity current with minimal disruption |
| Scrub Schedule | Sundays at 4:00 AM | Weekly integrity check catches bit rot early |
| SMART Check Schedule | Every 12 hours | Sufficient for drives under light load |
| Deletion Threshold | 50 | Catches accidental bulk deletes before parity is updated |
| Update Threshold | -1 | Disabled — home use rarely sees suspicious update spikes |
| Enable Pre-Hash | Off | Not needed for low-throughput arrays |
| Default Scrub Plan | 8% | Full array is verified roughly every 3 months |
| Scrub Min Age (days) | 10 | Avoids re-checking recently verified blocks |
| Auto-Fix Bad Blocks | Off | Review errors manually before repairing |

**Media Server (8-24 drives, frequent writes)**

| Setting | Value | Why |
|---------|-------|-----|
| Maintenance Schedule | Daily at 2:00 AM | Earlier window to finish before morning activity |
| Scrub Schedule | Sun & Wed at 4:00 AM | Twice-weekly scrub for larger arrays with more data churn |
| SMART Check Schedule | Every 6 hours | More frequent checks for drives under heavier load |
| Deletion Threshold | 100 | Higher limit for libraries where bulk imports/removals are normal |
| Update Threshold | 500 | Catch runaway processes but allow large media ingests |
| Enable Pre-Hash | On | Detects silent corruption during write-heavy workloads |
| Default Scrub Plan | 8% | Full array verified roughly every 3 months |
| Scrub Min Age (days) | 7 | Check blocks more frequently due to higher data churn |
| Auto-Fix Bad Blocks | Off | Investigate root cause before auto-repairing |
| Pause Containers | plex,sonarr,radarr | Prevents file changes during sync for media apps |

**Production / Archive (24+ drives, critical data)**

| Setting | Value | Why |
|---------|-------|-----|
| Maintenance Schedule | Daily at 2:00 AM | Consistent nightly parity updates |
| Scrub Schedule | Daily at 4:00 AM | Daily scrub for maximum data integrity assurance |
| SMART Check Schedule | Every 4 hours | Aggressive monitoring for early failure detection |
| Deletion Threshold | 25 | Low tolerance — any unexpected bulk delete is suspicious |
| Update Threshold | 200 | Flag unusual update volumes for review |
| Enable Pre-Hash | On | Essential for detecting silent corruption on archive data |
| Default Scrub Plan | Full | Verify entire array every pass |
| Scrub Min Age (days) | 3 | Minimize window for undetected bit rot |
| Auto-Fix Bad Blocks | On | Automated repair minimizes data-at-risk window on large arrays |
| Pre-Sync Hook | `/usr/local/bin/pre-sync.sh` | Run custom validation or flush caches before sync |

### Pre-Flight Safety Gates

Before any automated sync, four gates must pass:

0. **Config Files Gate** — Validates that all content and parity files referenced in the snapraid configuration exist and are non-empty.
1. **SMART Gate** — Aborts if any disk reports `FAIL`/`PREFAIL` or exceeds the failure probability threshold.
2. **Diff Threshold Gate** — Aborts if deleted or updated files exceed configured limits. The `add_del_ratio` can override a deletion breach.
3. **Concurrency Lock** — Ensures no other SnapRAID operation is running.

### Maintenance Pipeline

The full automated maintenance pipeline runs as:

```
config files gate → touch → diff → SMART gate → diff threshold gate →
  pre-sync hook → pause/stop containers → sync → unpause/start containers →
  post-sync hook → scrub → auto-fix (if bad blocks detected)
```

### Job Cancellation

Any running SnapRAID operation can be aborted from the Dashboard's Active Job progress card or via the API (`POST /api/command` with `action: "abort"`). The abort sends a termination signal to the underlying snapraid process.

## Notifications

The Hub emits notification frames upstream to the Vigil Server for dispatch through your configured channels (Discord, Telegram, email, Slack, etc. via Shoutrrr). You receive a notification for every operation, every automation run, and every failure.

### Notification Events

| Event | Severity | Trigger |
|-------|----------|---------|
| `job_started` | info | Any SnapRAID operation begins (manual or scheduled) |
| `job_complete` | info | Any SnapRAID operation finishes successfully |
| `job_failed` | critical | A command routed to an Agent fails |
| `smart_warning` | warning | SMART status reports `FAIL` or `PREFAIL` on any disk |
| `maintenance_started` | info | Scheduled maintenance pipeline begins |
| `maintenance_complete` | info | Scheduled maintenance pipeline finishes successfully |
| `gate_failed` | warning | A pre-flight safety gate aborts the maintenance pipeline |

### What You Get Notified About

- **Every sync, scrub, touch, diff, smart, fix, and status** operation — whether triggered manually from the Operations page or by the scheduler — emits `job_started` and `job_complete` notifications.
- **Scheduled maintenance** emits `maintenance_started` at the beginning and `maintenance_complete` at the end. If a safety gate blocks the pipeline, you get a `gate_failed` notification with the reason (e.g., "42 files deleted exceeds threshold of 25").
- **SMART failures** are detected during periodic health checks and emit `smart_warning` with the affected disk name and device.
- **Command failures** (e.g., network errors routing to an Agent) emit `job_failed` with the error details.

### Setup

Notifications are dispatched through the Vigil Server's notification system. Configure your notification channels (Discord webhook, Telegram bot, email, etc.) in the Vigil Server settings. The SnapRAID Hub automatically forwards all events upstream — no additional configuration is needed on the Hub or Agent side.

## API Reference

### Agent Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check |
| `POST` | `/api/execute` | Trigger a SnapRAID command (sync, scrub, fix, status, smart, diff, touch) |
| `POST` | `/api/abort` | Cancel the currently running SnapRAID operation |
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

The Agent validates all cron expressions on startup. Use one of the preset schedules from the Automation page, or if using the Custom option, ensure you use standard 5-field cron format:

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
