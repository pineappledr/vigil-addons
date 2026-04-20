# vigil-addons

Official and community add-ons for the Vigil infrastructure monitoring platform.

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-linux%2Famd64%20%7C%20linux%2Farm64-lightgrey)](https://github.com/pineappledr/vigil-addons)
[![Vigil](https://img.shields.io/badge/Requires-Vigil%20v3.0%2B-8b5cf6)](https://github.com/pineappledr/vigil)

The official add-on ecosystem for [Vigil](https://github.com/pineappledr/vigil) v3.0+. Each add-on is a standalone, multi-architecture background daemon that registers with the Vigil server, streams real-time telemetry over WebSockets, and renders a dynamic UI in the Vigil dashboard -- no frontend code required.

---

## Architecture Overview

Vigil add-ons follow a fully decoupled architecture. The Vigil server and each add-on communicate exclusively through a persistent WebSocket channel and a structured JSON manifest that declaratively describes the add-on's UI.

```
                                  WebSocket
┌────────────────────┐    (telemetry frames:       ┌────────────────────┐
│                    │     progress, logs,          │                    │
│   Vigil Server     │ ◄── notifications, ────────  │   Add-on Daemon    │
│   (Go + SQLite)    │     heartbeats)              │   (standalone Go   │
│                    │                              │    binary/image)   │
│  ┌──────────────┐  │                              │                    │
│  │ Dashboard UI │  │  ──── SSE (real-time) ─────► │  Runs on any host  │
│  │ (renders     │  │                              │  amd64 or arm64    │
│  │  manifest)   │  │                              └────────────────────┘
│  └──────────────┘  │
└────────────────────┘
```

**How it works:**

1. The admin registers an add-on from the Vigil dashboard (**Add-ons** > **Add Add-on**), which generates a registration token and add-on ID.
2. The add-on daemon starts, sends its JSON UI manifest via `POST /api/addons`, then opens a persistent WebSocket at `/api/addons/ws?addon_id=<ID>` to stream telemetry frames.
3. The Vigil server routes each frame to the dashboard via Server-Sent Events and, for notification frames, dispatches alerts through the configured channels (email, Discord, Telegram, etc.).
4. The add-on's JSON manifest declares its UI -- forms, progress bars, charts, log viewers, and data tables -- which the dashboard renders dynamically. No frontend modifications are needed.

---

## Available Add-ons

| Add-on | Description | Status | Documentation |
| ------ | ----------- | ------ | ------------- |
| **Disk Burn-in & Pre-clear** | Production-grade drive qualification tool with SMART testing, destructive badblocks, automated GPT partitioning, and ext4 formatting. | Stable | [burn-in/README.md](burn-in/README.md) |
| **SnapRAID** | Full lifecycle management of local SnapRAID arrays — native Go scheduling engine, safety gates, job history, and real-time log streaming. Replaces `snapraid-runner` and cron wrappers. | Stable | [snapraid/README.md](snapraid/README.md) |
| **ZFS Manager** | Visual ZFS pool, dataset, and snapshot management for Proxmox, Debian, Ubuntu, and bare-metal Linux hosts. Multi-agent: one manager, many ZFS hosts. | Phase 1 (v0.1.0) | [zfs-manager/README.md](zfs-manager/README.md) |

---

## Quick Start

### 1. Register in Vigil

Navigate to the Vigil dashboard > **Add-ons** > **Add Add-on**. Note the generated **Add-on ID** and **Token**.

### 2. Deploy the Add-on

```bash
# Clone the repository
git clone https://github.com/pineappledr/vigil-addons.git
cd vigil-addons

# See the specific add-on README for deployment instructions
```

Each add-on has its own `README.md` with Docker Compose examples and environment variable references.

---

## Building Your Own Add-on

See the **[Developer Guide](DEVELOPER_GUIDE.md)** for a complete walkthrough covering:

- JSON UI manifest structure and supported component types
- WebSocket communication protocol and frame formats
- Multi-architecture build requirements
- CI/CD pipeline conventions

---

## Repository Structure

```
vigil-addons/
├── README.md                       # This file
├── DEVELOPER_GUIDE.md              # How to build a new add-on
├── ROADMAP.md                      # Planned add-ons and features
├── LICENSE
├── shared/                         # Shared Go libraries
│   ├── addonutil/                  # HTTP helpers (WriteJSON, etc.)
│   └── vigilclient/                # Vigil registration + telemetry client
├── .github/
│   └── workflows/
│       ├── burnin.yml              # CI/CD for burn-in
│       ├── snapraid.yml            # CI/CD for snapraid
│       └── zfs-manager.yml        # CI/CD for zfs-manager
├── burn-in/                        # Disk Burn-in & Pre-clear add-on
│   ├── README.md
│   ├── Dockerfile.hub
│   ├── Dockerfile.agent
│   ├── go.mod
│   ├── cmd/
│   │   ├── hub/                    # Hub binary
│   │   └── agent/                  # Agent binary
│   └── internal/
│       ├── config/
│       ├── hub/
│       ├── agent/
│       ├── drive/
│       └── jobs/
├── snapraid/                       # SnapRAID add-on
│   ├── README.md
│   ├── Dockerfile.hub
│   ├── Dockerfile.agent
│   ├── go.mod
│   ├── cmd/
│   │   ├── hub/
│   │   └── agent/
│   └── internal/
│       ├── config/
│       ├── hub/
│       └── agent/
└── zfs-manager/                    # ZFS Manager add-on
    ├── README.md
    ├── Dockerfile.manager
    ├── Dockerfile.agent            # Alpine (default)
    ├── Dockerfile.agent.debian     # Debian/glibc for Proxmox hosts
    ├── go.mod
    ├── cmd/
    │   ├── manager/                # Manager binary + manifest.json
    │   └── agent/                  # Agent binary
    └── internal/
        ├── config/
        ├── manager/
        └── agent/
```

---

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.
