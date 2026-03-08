#!/usr/bin/env bash
# pre-sync.sh — Runs before each SnapRAID sync operation.
#
# Exit with non-zero to abort the maintenance pipeline.
# The Agent runs this inside its own environment (or container).
#
# Usage:
#   1. Customize the sections below for your setup.
#   2. Make executable: chmod +x pre-sync.sh
#   3. If Dockerized, mount into the agent container:
#        volumes:
#          - ./scripts/pre-sync.sh:/usr/local/bin/pre-sync.sh:ro
#   4. Set the path in Vigil UI → SnapRAID → Automation → Pre-Sync Hook.

set -euo pipefail

LOGPREFIX="[pre-sync]"

echo "$LOGPREFIX Starting pre-sync checks at $(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ─── Flush filesystem caches ──────────────────────────────────────────
# Forces pending writes to disk so snapraid sees consistent data.
# Uncomment if your workload does heavy buffered writes.
#
# sync
# echo 3 > /proc/sys/vm/drop_caches
# echo "$LOGPREFIX Filesystem caches flushed"

# ─── Send webhook notification ────────────────────────────────────────
# Notify an external service that sync is about to start.
# Replace the URL with your own webhook endpoint.
#
# curl -sf -X POST "https://hooks.example.com/snapraid" \
#   -H "Content-Type: application/json" \
#   -d "{\"event\":\"sync_starting\",\"timestamp\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" \
#   || echo "$LOGPREFIX WARNING: webhook notification failed (non-fatal)"

# ─── Stop non-Docker services ────────────────────────────────────────
# If you have services writing to data disks that aren't managed via
# Docker container pause/stop, stop them here.
#
# systemctl stop my-file-watcher.service
# echo "$LOGPREFIX Stopped file watcher service"

# ─── Custom validation ───────────────────────────────────────────────
# Add any pre-flight checks specific to your setup.
# Exit non-zero to abort the sync.
#
# if [ ! -f /mnt/parity/snapraid.parity ]; then
#   echo "$LOGPREFIX ERROR: parity file missing — aborting"
#   exit 1
# fi

echo "$LOGPREFIX Pre-sync checks passed"
