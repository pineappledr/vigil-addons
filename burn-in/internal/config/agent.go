package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentConfig holds all configuration for the burn-in agent.
type AgentConfig struct {
	Hub   AgentHubConfig   `json:"hub"`
	Agent AgentNodeConfig  `json:"agent"`
}

type AgentHubConfig struct {
	URL string `json:"url"`
	PSK string `json:"psk"`
}

type AgentNodeConfig struct {
	ID                string        `json:"id"`
	Listen            string        `json:"listen"`
	AdvertiseAddr     string        `json:"advertise_addr"`
	ServerPubkey      string        `json:"server_pubkey"`
	SmartPollInterval time.Duration `json:"smart_poll_interval"`
	TempPollInterval  time.Duration `json:"temp_poll_interval"`
	LogDir            string        `json:"log_dir"`
}

// LoadAgentConfig loads agent configuration from an optional JSON file
// and environment variable overrides.
func LoadAgentConfig() (*AgentConfig, error) {
	cfg := &AgentConfig{
		Hub: AgentHubConfig{
			URL: "http://localhost:9100",
		},
		Agent: AgentNodeConfig{
			Listen:            ":9200",
			SmartPollInterval: 10 * time.Second,
			TempPollInterval:  10 * time.Second,
			LogDir:            "/var/lib/vigil-agent/logs",
		},
	}

	if path := os.Getenv("BURNIN_CONFIG_FILE"); path != "" {
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	if v := os.Getenv("BURNIN_HUB_URL"); v != "" {
		cfg.Hub.URL = v
	}
	if v := os.Getenv("BURNIN_HUB_PSK"); v != "" {
		cfg.Hub.PSK = v
	}
	if v := os.Getenv("BURNIN_AGENT_ID"); v != "" {
		cfg.Agent.ID = v
	}
	if v := os.Getenv("BURNIN_AGENT_LISTEN"); v != "" {
		cfg.Agent.Listen = v
	}
	if v := os.Getenv("BURNIN_AGENT_ADVERTISE_ADDR"); v != "" {
		cfg.Agent.AdvertiseAddr = v
	}
	if v := os.Getenv("BURNIN_AGENT_SERVER_PUBKEY"); v != "" {
		cfg.Agent.ServerPubkey = v
	}
	if v := os.Getenv("BURNIN_AGENT_SMART_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid BURNIN_AGENT_SMART_POLL_INTERVAL: %w", err)
		}
		cfg.Agent.SmartPollInterval = d
	}
	if v := os.Getenv("BURNIN_AGENT_TEMP_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid BURNIN_AGENT_TEMP_POLL_INTERVAL: %w", err)
		}
		cfg.Agent.TempPollInterval = d
	}
	if v := os.Getenv("BURNIN_AGENT_LOG_DIR"); v != "" {
		cfg.Agent.LogDir = v
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

func (c *AgentConfig) validate() error {
	var missing []string

	if c.Hub.URL == "" {
		missing = append(missing, "hub.url")
	}
	if c.Hub.PSK == "" {
		missing = append(missing, "hub.psk")
	}
	if c.Agent.ID == "" {
		missing = append(missing, "agent.id")
	}
	if c.Agent.Listen == "" {
		missing = append(missing, "agent.listen")
	}

	if len(missing) > 0 {
		return fmt.Errorf("required config missing: %s", strings.Join(missing, ", "))
	}
	return nil
}
