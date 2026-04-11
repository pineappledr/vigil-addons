# ZFS Manager Add-on for Vigil

Visual ZFS pool, dataset, and snapshot management for homelab servers running Proxmox, Debian, Ubuntu, or any bare-metal Linux host with ZFS. Replaces the ZFS management panel you lost when you migrated away from TrueNAS.

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/Platform-linux%2Famd64%20%7C%20linux%2Farm64-lightgrey)](https://github.com/pineappledr/vigil-addons)

---

## Table of Contents

1. [Architecture](#architecture)
2. [Dashboard](#dashboard)
3. [Deployment](#deployment)
4. [Configuration Reference](#configuration-reference)
5. [API Reference](#api-reference)
6. [Security Model](#security-model)

---

## Architecture

The add-on uses a **Hub/Agent model** consistent with all Vigil add-ons:

```
Vigil Server (:9080)
    │  POST /api/addons/connect   (registration + manifest)
    │  WS   /api/addons/ws        (telemetry upstream)
    │
ZFS Manager (:9500)              ← runs anywhere, no ZFS required
    │  REST (register, telemetry ingest, UI data)
    │
ZFS Agent (:9600)   ← one per ZFS host (Proxmox, Debian, Ubuntu, etc.)
    │
    └─ zpool / zfs CLI (host binaries, bind-mounted)
```

| Component | Responsibility |
|-----------|----------------|
| **Manager** | Agent registry, telemetry aggregation, upstream Vigil connection, UI data endpoints |
| **Agent** | Executes `zpool`/`zfs` commands, streams telemetry (pools, datasets, snapshots) |

The Manager has no ZFS dependency and can run in Docker alongside the Vigil server. Agents run on each ZFS host and mount host ZFS binaries via Docker volumes (or run as a native binary on bare metal).

### Agent Authentication (PSK)

Because agents will eventually execute destructive operations, every agent request to the manager requires a **Pre-Shared Key** in the `Authorization: Bearer <psk>` header.

- The Manager generates a cryptographically random 32-byte hex PSK on first boot and saves it to `/data/hub.psk`.
- The **deploy-wizard** in the Vigil UI auto-fills the PSK into the generated Docker Compose — you never copy it manually.
- Rotate with `POST /api/rotate-psk` (type `ROTATE` to confirm). All agents must be redeployed with the new PSK.

---

## Dashboard

| Page | Description |
|------|-------------|
| **Pools** | Pool health (ONLINE/DEGRADED/FAULTED), size, used/free, fragmentation %, dedup ratio, last scrub date, vdev topology tree |
| **Datasets** | All ZFS filesystems — used, available, referenced, mountpoint, compression, record size, atime, sync |
| **Snapshots** | All snapshots across all datasets — creation date, used space, referenced size. Sortable by date. |
| **Replication** | Local `zfs send | receive` replication tasks — create, schedule, run manually, view history. Supports full and incremental sends with automatic common-snapshot resolution. |
| **Agents** | Registered agents, online/offline status, last-seen timestamp, deploy-wizard to add new agents |

All data pages have an **agent selector** — pick which host to view.

---

## Prerequisites

### Manager Requirements

The Manager is a lightweight coordinator — it has **no ZFS dependency** and can run on any host (including one without ZFS). Only needs:

- Docker (if using the container image), or any Linux amd64/arm64 host for the binary
- Network access to the Vigil Server and to Agent hosts

### Agent Host Requirements

Each host running a ZFS Agent **must** have ZFS installed and operational. The Agent does not bundle ZFS — it calls the host's `zpool` and `zfs` binaries directly.

#### Required

| Dependency | Provides | Used For |
|------------|----------|----------|
| **ZFS** (`zfsutils-linux` or equivalent) | `zpool`, `zfs` | All pool, dataset, snapshot, scrub, and replication operations |
| `/dev/zfs` device node | Kernel ZFS interface | Required for any `zpool`/`zfs` command to function |
| **Root/sudo access** | Privileged execution | ZFS operations require root. Docker containers must run with `privileged: true` |

Install ZFS on **Debian/Ubuntu/Proxmox**:

```bash
sudo apt install -y zfsutils-linux
```

Install ZFS on **Arch Linux** (via [archzfs](https://github.com/archzfs/archzfs)):

```bash
sudo pacman -S zfs-linux
```

Install ZFS on **Fedora/RHEL**:

```bash
sudo dnf install -y https://zfsonlinux.org/fedora/zfs-release-latest.noarch.rpm
sudo dnf install -y zfs
```

> **TrueNAS SCALE / Proxmox:** ZFS is pre-installed — no additional packages needed.

#### Optional

| Dependency | Provides | Used For |
|------------|----------|----------|
| **ledctl** (`ledmon` package) | `ledctl` | Drive bay LED identification (locate/fault blink). Gracefully skipped if not installed — the "Identify" button will not appear in the UI. |

```bash
# Debian/Ubuntu
sudo apt install -y ledmon

# Arch
sudo pacman -S ledmon
```

#### Docker Volume Mounts

When running the Agent in Docker, the host's ZFS binaries and libraries must be bind-mounted into the container:

| Mount | Purpose |
|-------|---------|
| `/dev/zfs:/dev/zfs` | Kernel ZFS device node |
| `/sbin/zpool:/sbin/zpool:ro` | ZFS pool management binary |
| `/sbin/zfs:/sbin/zfs:ro` | ZFS dataset/snapshot binary |
| `/lib:/lib:ro` | Shared libraries required by `zpool`/`zfs` |
| `/lib64:/lib64:ro` | 64-bit shared libraries |
| `/usr/lib:/usr/lib:ro` | Additional shared libraries |

> **Why `privileged: true`?** ZFS operations interact directly with kernel modules and block devices. Without privileged mode, `zpool` and `zfs` commands will fail with permission errors.

> **Alpine vs Debian image:** The default image (`latest`) is Alpine-based. If your host uses glibc-compiled ZFS (Proxmox, Ubuntu, Debian), use `latest-debian` to avoid dynamic linker errors when calling the mounted host binaries.

### Network Ports

| Port | Service | Direction |
|------|---------|-----------|
| 9080 | Vigil Server | Manager → Server |
| 9500 | ZFS Manager | Agent → Manager, Server → Manager |
| 9600 | ZFS Agent | Manager → Agent |

---

## Deployment

### 1. Register in Vigil

Go to the Vigil dashboard → **Add-ons** → **New Add-on**. Copy the token.

### 2. Deploy the Manager

```yaml
services:
  vigil-zfs-manager:
    image: ghcr.io/pineappledr/vigil-addons-zfs-manager:latest
    container_name: vigil-zfs-manager
    restart: unless-stopped
    ports:
      - "9500:9500"
    environment:
      VIGIL_URL: http://vigil:9080
      VIGIL_TOKEN: your-addon-token-here
      VIGIL_SERVER_PUBKEY: your-server-public-key
      VIGIL_ZFS_MANAGER_DATA_REGISTRY_PATH: "/data"
      TZ: ${TZ:-UTC}
    volumes:
      - zfs-manager-data:/data

volumes:
  zfs-manager-data:
```

On first start, the manager logs the PSK path:

```
{"level":"INFO","msg":"PSK ready","psk_path":"/data/hub.psk"}
```

### 3. Deploy Agents (via deploy-wizard)

In the Vigil dashboard, go to your ZFS Manager add-on → **Agents** tab → **Add ZFS Agent**. The wizard auto-fills the Manager URL and PSK. Fill in:

- **Agent ID** — a short name for the host (e.g. `nas-01`, `proxmox`)
- **Advertise Address** — URL the Manager can reach the agent at (e.g. `http://192.168.1.100:9600`)

#### Docker (Standard Linux / Alpine)

```yaml
services:
  vigil-zfs-agent:
    image: ghcr.io/pineappledr/vigil-addons-zfs-agent:latest
    container_name: vigil-zfs-agent
    restart: unless-stopped
    privileged: true
    ports:
      - "9600:9600"
    environment:
      VIGIL_ZFS_AGENT_HUB_URL: http://vigil-zfs-manager:9500
      VIGIL_ZFS_AGENT_HUB_PSK: <psk-from-wizard>
      VIGIL_ZFS_AGENT_ID: nas-01
      VIGIL_ZFS_AGENT_ADVERTISE_ADDR: http://192.168.1.100:9600
      VIGIL_ZFS_AGENT_LISTEN_PORT: "9600"
      TZ: America/New_York
    volumes:
      - zfs-agent-data:/data
      - /dev/zfs:/dev/zfs
      - /sbin/zpool:/sbin/zpool:ro
      - /sbin/zfs:/sbin/zfs:ro
      - /lib:/lib:ro
      - /lib64:/lib64:ro
      - /usr/lib:/usr/lib:ro

volumes:
  zfs-agent-data:
```

#### Docker (Proxmox / Ubuntu / Debian — glibc hosts)

Use the Debian image to avoid linker errors when host ZFS libraries are compiled against glibc:

```yaml
    image: ghcr.io/pineappledr/vigil-addons-zfs-agent:latest-debian
```

Everything else is identical.

#### Bare Metal (systemd)

```bash
# Download the agent binary
curl -sL https://github.com/pineappledr/vigil-addons/releases/latest/download/zfs-agent-linux-amd64 \
  -o /usr/local/bin/zfs-agent
chmod +x /usr/local/bin/zfs-agent
```

```ini
# /etc/systemd/system/vigil-zfs-agent.service
[Unit]
Description=Vigil ZFS Agent
After=network.target zfs.target

[Service]
Type=simple
Environment=VIGIL_ZFS_AGENT_HUB_URL=http://manager-host:9500
Environment=VIGIL_ZFS_AGENT_HUB_PSK=your-psk-here
Environment=VIGIL_ZFS_AGENT_ID=nas-01
Environment=VIGIL_ZFS_AGENT_ADVERTISE_ADDR=http://192.168.1.100:9600
ExecStart=/usr/local/bin/zfs-agent
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now vigil-zfs-agent
```

---

## Configuration Reference

### Manager

All values can be set via environment variables (prefix: `VIGIL_ZFS_MANAGER_`) or a `config.manager.yaml` file. Environment variables take precedence.

| Environment Variable | YAML Key | Default | Description |
|---|---|---|---|
| `VIGIL_ZFS_MANAGER_LISTEN_PORT` | `listen.port` | `9500` | HTTP port the manager listens on |
| `VIGIL_URL` | `vigil.server_url` | `http://vigil.local:9080` | Vigil server URL |
| `VIGIL_TOKEN` | `vigil.token` | — | Add-on registration token from Vigil UI |
| `VIGIL_SERVER_PUBKEY` | `vigil.server_pubkey` | — | Base64-encoded Ed25519 public key for command signature verification |
| `VIGIL_ZFS_MANAGER_DATA_REGISTRY_PATH` | `data.registry_path` | `/data/agents.json` | Agent registry file path |
| `VIGIL_ZFS_MANAGER_LOGGING_LEVEL` | `logging.level` | `info` | Log level (`debug`, `info`, `warn`, `error`) |

### Agent

All values can be set via environment variables (prefix: `VIGIL_ZFS_AGENT_`) or a `config.agent.yaml` file. The agent starts with defaults if no config file is present.

| Environment Variable | YAML Key | Default | Description |
|---|---|---|---|
| `VIGIL_ZFS_AGENT_LISTEN_PORT` | `listen.port` | `9600` | HTTP port the agent listens on |
| `VIGIL_ZFS_AGENT_HUB_URL` | `hub.url` | `http://zfs-manager:9500` | Manager URL |
| `VIGIL_ZFS_AGENT_HUB_PSK` | `hub.psk` | — | Pre-shared key (from manager `/data/hub.psk`) |
| `VIGIL_ZFS_AGENT_ID` | `identity.agent_id` | hostname | Unique name shown in the UI |
| `VIGIL_ZFS_AGENT_ADVERTISE_ADDR` | `identity.advertise_addr` | — | URL the manager uses to reach this agent |
| `VIGIL_ZFS_AGENT_ZFS_ZPOOL_PATH` | `zfs.zpool_path` | auto-detect | Path to `zpool` binary |
| `VIGIL_ZFS_AGENT_ZFS_ZFS_PATH` | `zfs.zfs_path` | auto-detect | Path to `zfs` binary |
| `VIGIL_ZFS_AGENT_LOGGING_LEVEL` | `logging.level` | `info` | Log level |

See [config.manager.example.yaml](config.manager.example.yaml) and [config.agent.example.yaml](config.agent.example.yaml) for annotated examples.

---

## API Reference

### Manager Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/health` | None | Health check |
| `GET` | `/api/deploy-info` | None | Returns `hub_url` + `hub_psk` for deploy-wizard |
| `POST` | `/api/agents/register` | PSK | Agent self-registration |
| `GET` | `/api/agents` | None | List all registered agents with status |
| `DELETE` | `/api/agents/{id}` | None | Remove an agent |
| `POST` | `/api/telemetry/ingest` | PSK | Receive telemetry from an agent |
| `GET` | `/api/telemetry/{agentID}` | None | Get cached telemetry for an agent |
| `GET` | `/api/pools` | None | Pool data for selected agent (`?agent_id=`) |
| `GET` | `/api/datasets` | None | Dataset data for selected agent (`?agent_id=`) |
| `GET` | `/api/snapshots` | None | Snapshot data for selected agent (`?agent_id=`) |
| `POST` | `/api/rotate-psk` | None | Rotate the PSK (body: `{"confirm":"ROTATE"}`) |
| `GET` | `/api/replication/tasks` | None | List replication tasks (proxied to agent) |
| `POST` | `/api/replication/tasks` | None | Create a replication task (proxied to agent) |
| `PUT` | `/api/replication/tasks/{id}` | None | Update a replication task (proxied to agent) |
| `DELETE` | `/api/replication/tasks/{id}` | None | Delete a replication task (proxied to agent) |
| `POST` | `/api/replication/tasks/{id}/run` | None | Manually trigger a replication task (proxied to agent) |
| `GET` | `/api/replication/tasks/{id}/history` | None | Replication task run history (proxied to agent) |

### Agent Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check |
| `GET` | `/api/telemetry` | Full telemetry snapshot |
| `GET` | `/api/pools` | Pool list |
| `GET` | `/api/datasets` | Dataset list |
| `GET` | `/api/snapshots` | Snapshot list |
| `GET` | `/api/replication/tasks` | List all replication tasks |
| `POST` | `/api/replication/tasks` | Create a replication task |
| `PUT` | `/api/replication/tasks/{id}` | Update a replication task |
| `DELETE` | `/api/replication/tasks/{id}` | Delete a replication task |
| `POST` | `/api/replication/tasks/{id}/run` | Manually trigger a replication run |
| `GET` | `/api/replication/tasks/{id}/history` | Replication run history |

---

## Security Model

| Layer | Mechanism |
|-------|-----------|
| **Vigil ↔ Manager** | Bearer token (`VIGIL_TOKEN`) over WebSocket |
| **Manager ↔ Agent** | Pre-shared key (PSK) via `Authorization: Bearer` header |
| **Command integrity** | Ed25519 signature verification on write operations (POST/PUT/DELETE). The Vigil server signs commands with its private key; the Manager verifies using `VIGIL_SERVER_PUBKEY`. |
| **PSK exposure** | PSK is redacted in all log output |
| **PSK compromise** | Rotate via `POST /api/rotate-psk`, redeploy agents with new PSK via deploy-wizard |
| **Agent image compatibility** | Use the `-debian` image on Proxmox/Ubuntu/Debian (glibc hosts) |

> **Note:** If `VIGIL_SERVER_PUBKEY` is not configured, signature verification is disabled. This is acceptable for development but **not recommended for production**.
