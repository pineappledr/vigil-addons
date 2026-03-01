# Disk Burn-in & Pre-clear Tool

A production-grade drive qualification daemon for the [Vigil](https://github.com/pineappledr/vigil) monitoring platform. Designed for homelabs, NAS builders, and data hoarders who need to stress-test and prepare drives before trusting them with data.

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/Platform-linux%2Famd64%20%7C%20linux%2Farm64-lightgrey)](https://github.com/pineappledr/vigil-addons)

---

## Table of Contents

1. [Architecture](#architecture)
2. [Execution Pipelines](#execution-pipelines)
3. [Deployment](#deployment)
4. [Configuration Reference](#configuration-reference)
5. [API Reference](#api-reference)
6. [Security Model](#security-model)

---

## Architecture

This tool uses a two-tier architecture that sits below the Vigil server:

```
┌─────────────────────────────────────────────────────────────────────┐
│  Tier 1: Vigil Server                                               │
│  Receives telemetry, renders UI, dispatches notifications           │
└──────────────────────────────┬──────────────────────────────────────┘
                               │ WebSocket (telemetry frames)
                               │ POST /api/addons (manifest registration)
┌──────────────────────────────▼──────────────────────────────────────┐
│  Tier 2: Burn-in Hub  (:9100)                                       │
│                                                                      │
│  ┌─────────────┐  ┌─────────────┐  ┌──────────────┐  ┌──────────┐  │
│  │ Agent       │  │ Command     │  │ Telemetry    │  │ Vigil    │  │
│  │ Registry    │  │ Router      │  │ Aggregator   │  │ Client   │  │
│  │ (JSON disk) │  │ (POST fwd)  │  │ (WS mux)    │  │ (WS up)  │  │
│  └─────────────┘  └─────────────┘  └──────────────┘  └──────────┘  │
└───────────┬──────────────────────────────┬──────────────────────────┘
            │ POST /api/agents/register    │ WebSocket (agent telemetry)
            │ POST /api/execute            │
┌───────────▼──────────────────────────────▼──────────────────────────┐
│  Tier 3: Burn-in Agent  (:9200)   [one per physical host]           │
│                                                                      │
│  ┌─────────────┐  ┌─────────────┐  ┌──────────────┐  ┌──────────┐  │
│  │ Drive       │  │ Job Manager │  │ REST API     │  │ Hub      │  │
│  │ Discovery   │  │ (dispatch,  │  │ (execute,    │  │ Telemetry│  │
│  │ (smartctl)  │  │  persist)   │  │  abort, sig) │  │ (WS out) │  │
│  └─────────────┘  └─────────────┘  └──────────────┘  └──────────┘  │
│                                                                      │
│  ┌──────────────────────────────────────────────────────────────┐    │
│  │ Execution Engines                                            │    │
│  │  drive/identify  drive/smart  drive/badblocks                │    │
│  │  drive/partition  drive/format  drive/verify                 │    │
│  │  jobs/burnin  jobs/preclear  jobs/manager                    │    │
│  └──────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
```

### Component Roles

| Component | Tier | Role |
|-----------|------|------|
| **Hub** | 2 | Central coordinator. Registers with the Vigil server, routes commands to agents, aggregates telemetry from all agents, evaluates notification triggers (temperature alerts, SMART warnings). |
| **Agent** | 3 | Runs on each physical host with drives to test. Discovers local drives, executes burn-in/pre-clear pipelines, streams progress to the Hub via WebSocket. |

### Communication Flow

1. **Agent** starts, discovers local drives via `/dev/disk/by-id/` + `smartctl`, registers with the Hub (`POST /api/agents/register`).
2. **Hub** registers with the Vigil server, transmitting its UI manifest and opening a persistent WebSocket.
3. User submits a job from the Vigil dashboard. The Hub routes the signed command to the target agent (`POST /api/execute`).
4. Agent verifies the Ed25519 signature, dispatches the job to the appropriate orchestrator, and streams telemetry back to the Hub.
5. Hub aggregates agent telemetry, evaluates notification triggers, and forwards everything upstream to Vigil.

---

## Execution Pipelines

### Burn-in (`burnin`)

A non-destructive-then-destructive pipeline that thoroughly tests a drive's health before trusting it with data.

| Phase | Description |
|-------|-------------|
| **PRE-FLIGHT** | Resolve the device path, verify it is not mounted or in a ZFS pool, capture baseline SMART snapshot (attributes 5, 196, 197, 198). |
| **SMART_SHORT** | Trigger `smartctl --test=short`, poll until completion using the drive's estimated polling time. Take a post-short SMART snapshot and compute deltas. Emit `SmartWarning` if any critical attribute increased. |
| **BADBLOCKS** | Execute `badblocks -wsv` (destructive 4-pattern write+verify). Stream progress across all 8 passes (4 patterns x write + read-back). Process group killing via `SIGKILL` on cancellation ensures the drive is not left in an indeterminate write state. |
| **SMART_EXTENDED** | Trigger `smartctl --test=long`, poll until completion. Take final SMART snapshot and compute full deltas against the baseline. |
| **COMPLETE** | Aggregate results. Pass if: both SMART tests passed, no bad blocks found, no critical attribute degradation. Emit `JobComplete` or `JobFailed`. |

### Pre-clear (`preclear`)

Prepares a drive for immediate use in a storage pool by partitioning, formatting, and verifying.

| Phase | Description |
|-------|-------------|
| **PRE-FLIGHT** | Same as burn-in: resolve device, safety check, baseline SMART snapshot. |
| **PARTITION** | Wipe existing partition table with `sgdisk --zap-all`, create a fresh GPT layout with a single partition spanning the entire drive (`sgdisk --new=1:0:0 --typecode=1:8300`). |
| **FORMAT** | Format the partition with `mkfs.ext4 -F -T largefile4 -m <reserved_pct>`. Stream formatting progress (inode tables, journal, superblocks). |
| **VERIFY** | Mount the partition read-only to a temp directory, run `dumpe2fs -h` to verify filesystem metadata integrity, unmount, then take a post-format SMART snapshot to detect hardware degradation caused by the write-heavy formatting process. |
| **COMPLETE** | Pass if: mount succeeded, metadata is clean, no SMART degradation. Emit `JobComplete` or `JobFailed`. |

### Full (`full`)

Runs the burn-in pipeline first. If burn-in passes, automatically transitions into the pre-clear pipeline using the same job ID and context. If burn-in fails, the job stops immediately without touching the partition table.

---

## Deployment

### Prerequisites

- A running [Vigil](https://github.com/pineappledr/vigil) v3.0+ server
- Docker and Docker Compose on each host (if using Docker deployment)
- Drives to test must be visible as block devices under `/dev/`
- Linux (amd64 or arm64)
- Root/sudo access (required for direct disk operations)

### System Requirements (Binary Install)

If you are running the binaries directly instead of Docker, the **Agent** host needs the following packages installed:

| Package | Provides | Used For |
|---------|----------|----------|
| `smartmontools` | `smartctl` | Drive identification, SMART attribute monitoring, short/long self-tests |
| `e2fsprogs` | `mkfs.ext4` | Formatting partitions with ext4 (pre-clear pipeline) |
| `e2fsprogs-extra` | `dumpe2fs` | Verifying ext4 filesystem metadata integrity (pre-clear pipeline) |
| `gptfdisk` | `sgdisk` | Wiping and creating GPT partition tables (pre-clear pipeline) |
| `util-linux` | `mount`, `umount` | Mounting/unmounting filesystems for verification (pre-clear pipeline) |

**Optional:**

| Package | Provides | Used For |
|---------|----------|----------|
| `zfsutils-linux` | `zpool` | Detecting drives that are part of an active ZFS pool (safety check). Gracefully skipped if not installed. |

Install on **Debian/Ubuntu**:

```bash
sudo apt install smartmontools e2fsprogs gptfdisk util-linux
```

Install on **Alpine** (used by the Docker images):

```bash
apk add smartmontools e2fsprogs e2fsprogs-extra util-linux gptfdisk
```

> **Note:** The **Hub** binary has no external tool dependencies — it only coordinates agents over the network.

### Docker Compose (Recommended)

Create a `docker-compose.yml` on the host where drives are physically connected:

```yaml
services:
  # ── Tier 2: Hub (one instance) ──────────────────────────
  burnin-hub:
    image: ghcr.io/pineappledr/vigil-addons-burnin-hub:latest
    container_name: burnin-hub
    restart: unless-stopped
    ports:
      - "9100:9100"
    environment:
      VIGIL_URL: "http://vigil-server:9080"
      VIGIL_AGENT_TOKEN: "your-vigil-addon-token"
      BURNIN_HUB_AGENT_PSK: "a-strong-shared-secret"
      BURNIN_HUB_DATA_DIR: "/data"
    volumes:
      - burnin-hub-data:/data

  # ── Tier 3: Agent (one per physical host) ───────────────
  burnin-agent:
    image: ghcr.io/pineappledr/vigil-addons-burnin-agent:latest
    container_name: burnin-agent
    restart: unless-stopped
    privileged: true                          # REQUIRED: direct disk access
    ports:
      - "9200:9200"
    environment:
      BURNIN_HUB_URL: "http://burnin-hub:9100"
      BURNIN_HUB_PSK: "a-strong-shared-secret"
      BURNIN_AGENT_ID: "agent-host01"
      BURNIN_AGENT_LISTEN: ":9200"
    volumes:
      - /dev:/dev                             # REQUIRED: pass through block devices
    devices:
      - /dev/sda
      - /dev/sdb
      # List each drive, or use privileged + /dev mount for full access

volumes:
  burnin-hub-data:
```

```bash
docker compose up -d
```

> **Why `privileged: true`?** The agent needs raw access to block devices for `smartctl`, `badblocks`, `sgdisk`, `mkfs.ext4`, and `mount` operations. Without privileged mode, these operations will fail with permission errors.

> **Why `/dev:/dev`?** The agent discovers drives by scanning `/dev/disk/by-id/` and resolving symlinks to the underlying block devices. The entire `/dev` tree must be visible inside the container.

### Multi-Host Deployment

For testing drives across multiple physical servers, run one Hub and multiple Agents:

```
┌─────────────────┐
│   Vigil Server  │
└────────┬────────┘
         │
┌────────▼────────┐
│   Burn-in Hub   │  (single instance, e.g., on your NAS)
└──┬─────────┬────┘
   │         │
┌──▼──┐  ┌──▼──┐
│Agent│  │Agent│    (one per physical host with drives)
│host1│  │host2│
└─────┘  └─────┘
```

Each agent registers with the Hub independently. The Hub aggregates all telemetry upstream.

### Pre-compiled Binaries

Retrieve the latest release for your architecture from the [Releases](https://github.com/pineappledr/vigil-addons/releases) page.

```bash
# Install the Hub
chmod +x burnin-hub-linux-amd64
sudo mv burnin-hub-linux-amd64 /usr/local/bin/burnin-hub

# Install the Agent
chmod +x burnin-agent-linux-amd64
sudo mv burnin-agent-linux-amd64 /usr/local/bin/burnin-agent
```

The Agent host requires these system packages: `smartmontools`, `e2fsprogs`, `fdisk`, `gdisk`.

---

## Configuration Reference

All configuration uses environment variables. Vigil-related variables use the `VIGIL_` prefix, while addon-specific variables use `BURNIN_`. An optional JSON config file can be loaded via `BURNIN_CONFIG_FILE`.

### Hub Environment Variables

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `BURNIN_CONFIG_FILE` | | no | Path to a JSON config file |
| `VIGIL_URL` | `http://localhost:8080` | no | Vigil server URL |
| `VIGIL_AGENT_TOKEN` | | no | Bearer token for Vigil registration (from the Vigil "Add Add-on" dialog) |
| `VIGIL_SERVER_PUBKEY` | | no | Path to Vigil server Ed25519 public key (see [Getting the Server Public Key](#getting-the-server-public-key)) |
| `BURNIN_HUB_LISTEN` | `:9100` | no | Hub listen address |
| `BURNIN_HUB_AGENT_PSK` | | **yes** | Pre-shared key for agent authentication |
| `BURNIN_HUB_DATA_DIR` | `/var/lib/vigil-hub` | no | Directory for persistent data (agent registry) |
| `BURNIN_HUB_HEARTBEAT_INTERVAL` | `30s` | no | Heartbeat interval to Vigil (Go duration) |
| `BURNIN_ALERTS_TEMP_WARNING_C` | `45` | no | Drive temperature warning threshold (Celsius) |
| `BURNIN_ALERTS_TEMP_CRITICAL_C` | `55` | no | Drive temperature critical threshold (Celsius) |

### Agent Environment Variables

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `BURNIN_CONFIG_FILE` | | no | Path to a JSON config file |
| `BURNIN_HUB_URL` | `http://localhost:9100` | no | Hub URL for registration and telemetry |
| `BURNIN_HUB_PSK` | | **yes** | Pre-shared key (must match Hub) |
| `BURNIN_AGENT_ID` | | **yes** | Unique agent identifier |
| `BURNIN_AGENT_LISTEN` | `:9200` | no | Agent REST API listen address |
| `BURNIN_AGENT_ADVERTISE_ADDR` | | no | Address the Hub uses to reach this agent |
| `BURNIN_AGENT_SERVER_PUBKEY` | | no | Path to Hub's Ed25519 public key for command signature verification |
| `BURNIN_AGENT_SMART_POLL_INTERVAL` | `10s` | no | SMART polling interval (Go duration) |
| `BURNIN_AGENT_TEMP_POLL_INTERVAL` | `10s` | no | Temperature polling interval (Go duration) |

---

## API Reference

### Hub Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/health` | none | Liveness check |
| `POST` | `/api/agents/register` | PSK | Agent registration with drive inventory |
| `GET` | `/api/agents` | PSK | List registered agents |
| `POST` | `/api/execute` | PSK | Forward a signed command to an agent |
| `GET` | `/api/agents/{id}/telemetry` | PSK | WebSocket: agent telemetry stream |

### Agent Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/health` | none | Liveness check |
| `POST` | `/api/execute` | Ed25519 signature | Receive and execute a signed command |
| `DELETE` | `/api/jobs/{id}` | none | Abort a running job |

### Execute Payload

```json
{
  "agent_id": "agent-host01",
  "command": "burnin",
  "target": "/dev/disk/by-id/ata-WDC_WD80EFAX-68KNBN0_ABCDEF",
  "params": {
    "block_size": 4096,
    "concurrent_blocks": 65536,
    "abort_on_error": true,
    "reserved_pct": 1
  },
  "signature": "<base64-encoded-ed25519-signature>"
}
```

---

## Security Model

| Layer | Mechanism |
|-------|-----------|
| **Vigil ↔ Hub** | Bearer token (`VIGIL_AGENT_TOKEN`) over WebSocket |
| **Hub ↔ Agent** | Pre-shared key (`BURNIN_HUB_AGENT_PSK`) via `Authorization: Bearer` header |
| **Command integrity** | Ed25519 signature verification on execute payloads. The signature covers all fields except `signature` itself, re-marshaled deterministically. |
| **Drive safety** | Every operation checks that the target device is not mounted and not part of an active ZFS pool before proceeding. Conflict detection prevents two jobs from targeting the same drive simultaneously. |
| **Process isolation** | All subprocesses (`smartctl`, `badblocks`, `sgdisk`, `mkfs.ext4`) are launched with `Setpgid: true` and killed via process group `SIGKILL` on context cancellation. Arguments are passed as discrete `exec.Command` parameters -- never interpolated into shell strings. |

### Getting the Server Public Key

The `VIGIL_SERVER_PUBKEY` (for the Hub) and `BURNIN_AGENT_SERVER_PUBKEY` (for the Agent) are used for Ed25519 command signature verification. These keys are generated by the Vigil server.

To obtain the public key from your Vigil server:

```bash
# Copy the public key from the Vigil server's data directory
cp /path/to/vigil/data/server.pub /etc/vigil/server.pub
```

Then point the environment variable to the file:

```bash
VIGIL_SERVER_PUBKEY="/etc/vigil/server.pub"
```

> **Note:** If no public key is configured, signature verification is disabled. This is acceptable for development but **not recommended for production**.
