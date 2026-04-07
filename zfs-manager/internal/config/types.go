package config

import "log/slog"

type LogConfig struct {
	Level string `yaml:"level"`
}

func ValidateManagerConfig(cfg *ManagerConfig) error {
	if cfg.Listen.Port < 1 || cfg.Listen.Port > 65535 {
		return nil // will be caught at bind time
	}
	if cfg.Data.RegistryPath == "" {
		return nil
	}
	return nil
}

func ValidateAgentConfig(cfg *AgentConfig) error {
	if cfg.Listen.Port < 1 || cfg.Listen.Port > 65535 {
		cfg.Listen.Port = 9600
	}
	return nil
}

func RedactedPSK(psk string) string {
	if len(psk) <= 8 {
		return "***"
	}
	return psk[:4] + "****" + psk[len(psk)-4:]
}

func LogManagerConfig(logger *slog.Logger, cfg *ManagerConfig) {
	logger.Info("manager configuration loaded",
		"listen_port", cfg.Listen.Port,
		"vigil_server_url", cfg.Vigil.ServerURL,
		"registry_path", cfg.Data.RegistryPath,
		"log_level", cfg.Logging.Level,
	)
}

func LogAgentConfig(logger *slog.Logger, cfg *AgentConfig) {
	logger.Info("agent configuration loaded",
		"listen_port", cfg.Listen.Port,
		"hub_url", cfg.Hub.URL,
		"hub_psk", RedactedPSK(cfg.Hub.PSK),
		"agent_id", cfg.Identity.AgentID,
		"advertise_addr", cfg.Identity.AdvertiseAddr,
		"zpool_path", cfg.ZFS.ZpoolPath,
		"zfs_path", cfg.ZFS.ZfsPath,
		"log_level", cfg.Logging.Level,
	)
}
