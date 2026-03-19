package agent

import (
	"database/sql"
	"log/slog"
	"strconv"
	"strings"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	agentdb "github.com/pineappledr/vigil-addons/snapraid/internal/db"
)

// HydrateConfigFromCache overwrites fields in cfg with any values previously
// persisted to the config_cache table. This closes the restart gap: the UI
// writes user-chosen settings to SQLite, and this function replays them into
// the in-memory config before the scheduler starts.
func HydrateConfigFromCache(db *sql.DB, cfg *config.AgentConfig, logger *slog.Logger) error {
	cached, err := agentdb.GetAllCacheValues(db)
	if err != nil {
		return err
	}
	if len(cached) == 0 {
		logger.Info("config hydration: no cached values found, using defaults")
		return nil
	}

	applied := 0
	for key, value := range cached {
		if applyCacheValue(cfg, key, value) {
			logger.Info("config hydration: applied cached value", "key", key, "value", value)
			applied++
		} else {
			logger.Debug("config hydration: unrecognised cache key, skipping", "key", key)
		}
	}
	logger.Info("config hydration complete", "applied", applied, "total_cached", len(cached))
	return nil
}

// applyCacheValue maps a single cache key to the corresponding AgentConfig
// field. Returns true if the key was recognised and applied.
// The key names match the keys sent by the UI via POST /api/config.
func applyCacheValue(cfg *config.AgentConfig, key, value string) bool {
	switch key {
	// ── Scheduler ────────────────────────────────────────────────
	case "maintenance_cron":
		cfg.Scheduler.MaintenanceCron = value
	case "scrub_cron":
		cfg.Scheduler.ScrubCron = value
	case "status_cron":
		cfg.Scheduler.StatusCron = value

	// ── Thresholds ──────────────────────────────────────────────
	case "max_deleted":
		setInt(value, &cfg.Thresholds.MaxDeleted)
	case "max_updated":
		setInt(value, &cfg.Thresholds.MaxUpdated)
	case "add_del_ratio":
		setFloat(value, &cfg.Thresholds.AddDelRatio)
	case "smart_fail_probability":
		setInt(value, &cfg.Thresholds.SmartFailProbability)

	// ── Scrub ───────────────────────────────────────────────────
	case "scrub_plan":
		cfg.Scrub.Plan = value
	case "older_than_days":
		setInt(value, &cfg.Scrub.OlderThanDays)
	case "auto_fix_bad_blocks":
		cfg.Scrub.AutoFixBadBlocks = parseBool(value)

	// ── Sync ────────────────────────────────────────────────────
	case "pre_hash":
		cfg.Sync.PreHash = parseBool(value)

	// ── Hooks ───────────────────────────────────────────────────
	case "pre_sync":
		cfg.Hooks.PreSync = value
	case "post_sync":
		cfg.Hooks.PostSync = value

	// ── Docker ──────────────────────────────────────────────────
	case "pause_containers":
		cfg.Docker.PauseContainers = splitCSV(value)
	case "stop_containers":
		cfg.Docker.StopContainers = splitCSV(value)

	// ── Logging ─────────────────────────────────────────────────
	case "logging_level":
		cfg.Logging.Level = value
	case "logging_file":
		cfg.Logging.File = value
	case "logging_max_size_mb":
		setInt(value, &cfg.Logging.MaxSizeMB)
	case "logging_max_backups":
		setInt(value, &cfg.Logging.MaxBackups)

	default:
		return false
	}
	return true
}

func setInt(s string, dst *int) {
	if n, err := strconv.Atoi(s); err == nil {
		*dst = n
	}
}

func setFloat(s string, dst *float64) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*dst = f
	}
}

func parseBool(s string) bool {
	return s == "true" || s == "1"
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
