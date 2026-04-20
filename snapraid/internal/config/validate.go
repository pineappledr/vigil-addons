package config

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/robfig/cron/v3"
)

// cronParser validates standard 5-field cron expressions by prepending "0 " for the seconds field.
var cronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func validateCron(name, expr string) error {
	if expr == "" {
		return nil
	}
	_, err := cronParser.Parse("0 " + expr)
	if err != nil {
		return fmt.Errorf("invalid cron expression for %s (%q): %w", name, expr, err)
	}
	return nil
}

// ValidateAgentConfig checks that all cron expressions and required paths are valid.
func ValidateAgentConfig(cfg *AgentConfig) error {
	crons := []struct {
		name string
		expr string
	}{
		{"scheduler.maintenance_cron", cfg.Scheduler.MaintenanceCron},
		{"scheduler.scrub_cron", cfg.Scheduler.ScrubCron},
		{"scheduler.status_cron", cfg.Scheduler.StatusCron},
	}
	for _, c := range crons {
		if err := validateCron(c.name, c.expr); err != nil {
			return err
		}
	}

	if cfg.Listen.Port < 1 || cfg.Listen.Port > 65535 {
		return fmt.Errorf("listen.port %d is out of range (1-65535)", cfg.Listen.Port)
	}

	if cfg.SnapRAID.ConfigPath == "" {
		return fmt.Errorf("snapraid.config_path must not be empty")
	}

	return nil
}

// ValidateHubConfig checks that required Hub fields are valid.
func ValidateHubConfig(cfg *HubConfig) error {
	if cfg.Listen.Port < 1 || cfg.Listen.Port > 65535 {
		return fmt.Errorf("listen.port %d is out of range (1-65535)", cfg.Listen.Port)
	}

	if cfg.Data.RegistryPath == "" {
		return fmt.Errorf("data.registry_path must not be empty")
	}

	return nil
}

// RedactedToken returns a redacted version of a token for safe logging.
func RedactedToken(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + strings.Repeat("*", len(token)-8) + token[len(token)-4:]
}

// LogHubConfig logs the Hub configuration with sensitive values redacted.
func LogHubConfig(logger *slog.Logger, cfg *HubConfig) {
	logger.Info("hub configuration loaded",
		"listen_port", cfg.Listen.Port,
		"vigil_server_url", cfg.Vigil.ServerURL,
		"vigil_token", RedactedToken(cfg.Vigil.Token),
		"registry_path", cfg.Data.RegistryPath,
		"log_level", cfg.Logging.Level,
	)
}

// LogAgentConfig logs the Agent configuration with sensitive values redacted.
func LogAgentConfig(logger *slog.Logger, cfg *AgentConfig) {
	logger.Info("agent configuration loaded",
		"listen_port", cfg.Listen.Port,
		"hub_url", cfg.Hub.URL,
		"hub_psk", RedactedToken(cfg.Hub.PSK),
		"agent_id", cfg.Identity.AgentID,
		"advertise_addr", cfg.Identity.AdvertiseAddr,
		"snapraid_binary", cfg.SnapRAID.BinaryPath,
		"snapraid_config", cfg.SnapRAID.ConfigPath,
		"maintenance_cron", cfg.Scheduler.MaintenanceCron,
		"scrub_cron", cfg.Scheduler.ScrubCron,
		"status_cron", cfg.Scheduler.StatusCron,
		"log_level", cfg.Logging.Level,
	)
}
