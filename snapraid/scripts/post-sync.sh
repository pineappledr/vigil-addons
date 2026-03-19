#!/usr/bin/env bash
# post-sync.sh — Runs after each SnapRAID sync operation completes.
#
# This runs regardless of whether the sync succeeded or failed.
# Exit code is logged but does NOT affect the pipeline outcome.
#
# Usage:
#   1. Customize the sections below for your setup.
#   2. Make executable: chmod +x post-sync.sh
#   3. If Dockerized, mount into the agent container:
#        volumes:
#          - ./scripts/post-sync.sh:/usr/local/bin/post-sync.sh:ro
#   4. Set the path in Vigil UI → SnapRAID → Automation → Post-Sync Hook.

set -euo pipefail

LOGPREFIX="[post-sync]"

echo "$LOGPREFIX Starting post-sync tasks at $(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ─── Send completion webhook ─────────────────────────────────────────
# Notify an external service that sync has finished.
#
# curl -sf -X POST "https://hooks.example.com/snapraid" \
#   -H "Content-Type: application/json" \
#   -d "{\"event\":\"sync_complete\",\"timestamp\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" \
#   || echo "$LOGPREFIX WARNING: webhook notification failed (non-fatal)"

# ─── Backup parity files ─────────────────────────────────────────────
# Copy parity to an offsite or secondary location after each sync.
#
# rsync -a --delete /mnt/parity/ /mnt/backup/parity/
# echo "$LOGPREFIX Parity backup completed"

# ─── Backup content files ────────────────────────────────────────────
# SnapRAID content files are small but critical — back them up.
#
# cp /var/snapraid/snapraid.content /mnt/backup/snapraid-content-$(date +%Y%m%d)
# echo "$LOGPREFIX Content file backed up"

# ─── Restart non-Docker services ────────────────────────────────────
# If you stopped services in pre-sync.sh, restart them here.
#
# systemctl start my-file-watcher.service
# echo "$LOGPREFIX Restarted file watcher service"

# ─── Update monitoring ───────────────────────────────────────────────
# Push a metric to your monitoring system (Prometheus pushgateway, etc.)
#
# echo "snapraid_last_sync_epoch $(date +%s)" | \
#   curl -sf --data-binary @- "http://pushgateway:9091/metrics/job/snapraid" \
#   || echo "$LOGPREFIX WARNING: monitoring update failed (non-fatal)"

# ─── Healthcheck ping ────────────────────────────────────────────────
# Ping a dead man's switch / healthcheck service to confirm sync ran.
#
# curl -sf "https://hc-ping.com/YOUR-UUID-HERE" \
#   || echo "$LOGPREFIX WARNING: healthcheck ping failed (non-fatal)"

echo "$LOGPREFIX Post-sync tasks completed"
