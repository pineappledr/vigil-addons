# Disk Burn-in & Pre-clear Tool

A production-grade drive qualification daemon for the [Vigil](https://github.com/pineappledr/vigil) monitoring platform. Designed for homelabs, NAS builders, and data hoarders who need to stress-test and prepare drives before trusting them with data.

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/Platform-linux%2Famd64%20%7C%20linux%2Farm64-lightgrey)](https://github.com/pineappledr/vigil-addons)

---

## Table of Contents

1. [Architecture](#architecture)
2. [Execution Pipelines](#execution-pipelines)
3. [Job Parameters](#job-parameters)
4. [Deployment](#deployment)
5. [Configuration Reference](#configuration-reference)
6. [API Reference](#api-reference)
7. [Security Model](#security-model)

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

## Job Parameters

When starting a new job from the Vigil dashboard, the following parameters control the burn-in and pre-clear behavior:

### Common Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| **Job Type** | select | yes | `burnin` (burn-in only), `preclear` (pre-clear only), or `full` (burn-in then pre-clear) |
| **Agent** | select | yes | The burn-in agent to execute the job on. Agents are auto-discovered from the Hub's registry. |
| **Target Drive** | select | yes | The drive to test. Populated from the selected agent's discovered drives. Drives are identified by `/dev/disk/by-id/` path for stability across reboots. |
| **Confirm Destructive** | checkbox | yes | Safety gate — you must acknowledge that the operation will destroy all data on the selected drive. Triggers a multi-step confirmation (password + type "CONFIRM"). |

### Burn-in Parameters

These parameters apply to `burnin` and `full` job types. They control the `badblocks` phase, which is the longest and most I/O-intensive part of the pipeline.

| Parameter | Default | Description |
|-----------|---------|-------------|
| **Block Size (bytes)** | `4096` | The size of each data block written to and read back from the disk during the destructive write+verify test. **4096 bytes (4 KB)** matches the physical sector size of modern drives (both HDD and SSD). Using the native sector size ensures each physical sector is individually tested. Smaller values increase test granularity but take longer; larger values (e.g., 65536) speed up testing at the cost of less precise error localization. For most users, the default is optimal. |
| **Concurrent Blocks** | `65536` | How many blocks are held in memory and processed in parallel. This controls the I/O buffer size: `block_size × concurrent_blocks` = total buffer. At defaults, this is `4096 × 65536 = 256 MB`. Higher values keep the drive's command queue full, maximizing throughput and ensuring the drive is under sustained stress. Lower values reduce RAM usage, which may be useful on memory-constrained systems. The value does not affect test accuracy — only speed and memory usage. |
| **Abort on first error** | `true` | If enabled, the burn-in job stops immediately when the first bad block is detected. If disabled, the job continues through all 8 passes (4 patterns × write + read-back) and reports the total error count at the end. Disabling this is useful to assess the overall health of a drive with known issues. |

#### How Block Size and Concurrent Blocks Interact

```
Total buffer = block_size × concurrent_blocks

Examples:
  4096 × 65536  = 256 MB   ← default, good for most systems
  4096 × 32768  = 128 MB   ← lower RAM usage
  4096 × 131072 = 512 MB   ← faster on drives with large caches
  65536 × 4096  = 256 MB   ← same RAM, but 64KB blocks (less granular)
```

The badblocks phase writes 4 different patterns to every block on the disk, then reads each one back to verify. This means the **entire disk capacity is written and read 4 times** (8 total passes). For a 10 TB drive at typical HDD speeds (~200 MB/s), this takes approximately **55 hours**.

### Pre-clear Parameters

These parameters apply to `preclear` and `full` job types.

| Parameter | Default | Range | Description |
|-----------|---------|-------|-------------|
| **Reserved Space (%)** | `0` | 0–50 | Percentage of the formatted partition reserved for the root user (passed to `mkfs.ext4 -m`). Setting this to 0 makes the entire disk available to normal users. A small value (1–5%) is recommended for system drives; 0% is typical for data-only drives in a NAS or storage pool. |

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

### Step 1: Register the Add-on in Vigil

1. Open the Vigil dashboard and navigate to **Add-ons**.
2. Click **Add Add-on**.
3. Select the **Docker** tab.
4. Fill in:
   - **Name**: `Burn-In`
   - **Docker Image**: `ghcr.io/pineappledr/vigil-addons-burnin-hub`
5. Note the auto-populated **Server URL**, **Public Key**, and **Token** — these are your `VIGIL_URL`, `VIGIL_SERVER_PUBKEY`, and `VIGIL_AGENT_TOKEN` values.
6. Click **Copy docker compose** to get a pre-filled `docker-compose.yml`.

### Step 2: Deploy the Hub

Take the copied docker-compose, add your Hub-specific configuration, and deploy it:

```yaml
services:
  burnin-hub:
    image: ghcr.io/pineappledr/vigil-addons-burnin-hub:latest
    container_name: burnin-hub
    restart: unless-stopped
    ports:
      - "9100:9100"
    environment:
      VIGIL_URL: "http://vigil-server:9080"            # from the Add Add-on dialog
      VIGIL_AGENT_TOKEN: "your-vigil-addon-token"       # from the Add Add-on dialog
      VIGIL_SERVER_PUBKEY: "your-server-public-key"     # from the Add Add-on dialog
      BURNIN_HUB_ADVERTISE_URL: "http://add-ons-external:9100"  # externally-reachable Hub URL
      BURNIN_HUB_DATA_DIR: "/data"
      TZ: ${TZ:-UTC}
    volumes:
      - burnin-hub-data:/data

volumes:
  burnin-hub-data:
```

```bash
docker compose up -d
```

Go back to the Vigil dialog, enter the **Add-on URL** (e.g., `http://add-ons-external:9100`), and click **Register Add-on**. This binds the token to the add-on entry. The Hub will automatically connect, submit its manifest, and appear as **online** in the Add-ons list.

> **Note:** The Hub retries registration with exponential backoff. You can deploy it before or after clicking "Register Add-on" — it will connect as soon as the token is bound.

### Step 3: Deploy Agents

Each physical host with drives to test needs a burn-in agent. You can add agents from the Vigil dashboard:

1. Open the **Burn-In** add-on in Vigil.
2. Navigate to the **Agents** tab.
3. The **Add Burn-in Agent** section shows your Hub URL and PSK pre-filled.
4. Enter an **Agent ID** (e.g., `agent-nas-01`).
5. Click **Copy docker compose**.
6. Deploy on the target host:

```yaml
services:
  burnin-agent:
    image: ghcr.io/pineappledr/vigil-addons-burnin-agent:latest
    container_name: burnin-agent
    restart: unless-stopped
    privileged: true
    ports:
      - "9200:9200"
    environment:
      BURNIN_HUB_URL: "http://addon-host:9100"     # pre-filled by the deploy wizard
      BURNIN_HUB_PSK: "<auto-generated>"               # pre-filled by the deploy wizard
      BURNIN_AGENT_ID: "agent-nas-01"
      BURNIN_AGENT_LISTEN: ":9200"
      TZ: ${TZ:-UTC}
    volumes:
      - /dev:/dev
```

```bash
docker compose up -d
```

The agent will discover local drives, register with the Hub, and appear in the Agents table.

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

Repeat [Step 3](#step-3-deploy-agents) on each host. Each agent registers with the Hub independently. The Hub aggregates all telemetry upstream.

### Binary: Systemd Service (Recommended)

Deploy the Hub and Agent as systemd services for automatic startup, restart on failure, and proper log management.

#### 1. Download the Binaries

Download the latest release and install to `/usr/local/bin`:

```bash
# Hub (amd64)
curl -sL https://github.com/pineappledr/vigil-addons/releases/latest/download/burnin-hub-linux-amd64 \
  -o burnin-hub && chmod +x burnin-hub && sudo mv burnin-hub /usr/local/bin/

# Hub (arm64)
curl -sL https://github.com/pineappledr/vigil-addons/releases/latest/download/burnin-hub-linux-arm64 \
  -o burnin-hub && chmod +x burnin-hub && sudo mv burnin-hub /usr/local/bin/
```

```bash
# Agent (amd64 — requires system packages, see System Requirements above)
curl -sL https://github.com/pineappledr/vigil-addons/releases/latest/download/burnin-agent-linux-amd64 \
  -o burnin-agent && chmod +x burnin-agent && sudo mv burnin-agent /usr/local/bin/

# Agent (arm64)
curl -sL https://github.com/pineappledr/vigil-addons/releases/latest/download/burnin-agent-linux-arm64 \
  -o burnin-agent && chmod +x burnin-agent && sudo mv burnin-agent /usr/local/bin/
```

#### 2. Create the Hub Service

Create the environment file with your configuration:

```bash
sudo mkdir -p /etc/burnin
sudo tee /etc/burnin/hub.env > /dev/null <<'EOF'
VIGIL_URL=http://YOUR_VIGIL_SERVER:9080
VIGIL_AGENT_TOKEN=your-vigil-addon-token
VIGIL_SERVER_PUBKEY=your-server-public-key
BURNIN_HUB_LISTEN=:9100
BURNIN_HUB_ADVERTISE_URL=http://THIS_HOST_IP:9100
BURNIN_HUB_DATA_DIR=/var/lib/burnin-hub
EOF
```

Create the systemd unit:

```bash
sudo tee /etc/systemd/system/burnin-hub.service > /dev/null <<'EOF'
[Unit]
Description=Burn-in Hub
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/burnin/hub.env
ExecStart=/usr/local/bin/burnin-hub
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
```

Enable and start:

```bash
sudo mkdir -p /var/lib/burnin-hub
sudo systemctl daemon-reload
sudo systemctl enable --now burnin-hub
```

> The Hub auto-generates its agent PSK on first startup and stores it in `{data_dir}/hub.psk`. The deploy wizard on the Agents tab will pre-fill this key for you.

#### 3. Create the Agent Service

Create the environment file:

```bash
sudo tee /etc/burnin/agent.env > /dev/null <<'EOF'
BURNIN_HUB_URL=http://HUB_HOST_IP:9100
BURNIN_HUB_PSK=<copy from Hub's /var/lib/burnin-hub/hub.psk>
BURNIN_AGENT_ID=agent-host01
BURNIN_AGENT_LISTEN=:9200
EOF
```

Create the systemd unit:

```bash
sudo tee /etc/systemd/system/burnin-agent.service > /dev/null <<'EOF'
[Unit]
Description=Burn-in Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/burnin/agent.env
ExecStart=/usr/local/bin/burnin-agent
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now burnin-agent
```

#### Checking Status and Logs

```bash
# Service status
sudo systemctl status burnin-hub
sudo systemctl status burnin-agent

# Follow logs
sudo journalctl -u burnin-hub -f
sudo journalctl -u burnin-agent -f
```

#### Upgrading Binary Services

```bash
# 1. Stop the service
sudo systemctl stop burnin-hub   # or burnin-agent

# 2. Download and replace the binary
curl -sL https://github.com/pineappledr/vigil-addons/releases/latest/download/burnin-hub-linux-amd64 \
  -o burnin-hub && chmod +x burnin-hub && sudo mv burnin-hub /usr/local/bin/

# 3. Restart
sudo systemctl start burnin-hub   # or burnin-agent
```

### Binary: Manual (Foreground)

For quick testing or development, you can run the binaries directly:

```bash
# Hub
export VIGIL_URL="http://vigil-server:9080"
export VIGIL_AGENT_TOKEN="your-token"
export VIGIL_SERVER_PUBKEY="your-server-public-key"
export BURNIN_HUB_ADVERTISE_URL="http://addon-host:9100"
burnin-hub
```

```bash
# Agent
export BURNIN_HUB_URL="http://addon-host:9100"
export BURNIN_HUB_PSK="<copy from Hub's data_dir/hub.psk>"
export BURNIN_AGENT_ID="agent-host01"
burnin-agent
```

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
| `BURNIN_HUB_ADVERTISE_URL` | | no | Externally-reachable Hub URL (used by the deploy wizard to pre-fill agent config) |
| `BURNIN_HUB_AGENT_PSK` | *(auto-generated)* | no | Pre-shared key for agent authentication. If not set, the Hub generates a random 32-byte key on first startup and persists it to `{data_dir}/hub.psk`. |
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
| `GET` | `/api/agents` | none | List registered agents (used by Vigil proxy for UI) |
| `POST` | `/api/execute` | PSK | Forward a signed command to an agent |
| `GET` | `/api/agents/{id}/telemetry` | PSK | WebSocket: agent telemetry stream |
| `GET` | `/api/deploy-info` | none | Returns Hub URL and PSK for the deploy wizard |

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

| Field | Type | Description |
|-------|------|-------------|
| `agent_id` | string | Target agent to execute the command on |
| `command` | string | One of `burnin`, `preclear`, or `full` |
| `target` | string | Block device path (preferably `/dev/disk/by-id/...` for stability) |
| `params.block_size` | int | Block size in bytes for badblocks (see [Job Parameters](#job-parameters)) |
| `params.concurrent_blocks` | int | Number of concurrent blocks for I/O buffering |
| `params.abort_on_error` | bool | Stop on first bad block if true |
| `params.reserved_pct` | int | Reserved space percentage for mkfs.ext4 (pre-clear only) |
| `signature` | string | Base64-encoded Ed25519 signature over the payload |
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

**Via the Vigil UI (recommended):** The "Add Add-on" dialog shows the server public key as a base64 string. The generated docker-compose includes it automatically:

```bash
VIGIL_SERVER_PUBKEY: "a3+wIbEmVdCgaTCeNgT5cye6WCSks9H/4UjaT4vwYpk="
```

**Manual setup:** Copy the public key file from the Vigil server's data directory and point the environment variable to it:

```bash
cp /path/to/vigil/data/server.pub /etc/vigil/server.pub
VIGIL_SERVER_PUBKEY="/etc/vigil/server.pub"
```

> **Note:** If no public key is configured, signature verification is disabled. This is acceptable for development but **not recommended for production**.
