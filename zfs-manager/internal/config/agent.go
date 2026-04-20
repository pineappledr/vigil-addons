package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type AgentConfig struct {
	Listen   AgentListen   `yaml:"listen"`
	Hub      AgentHub      `yaml:"hub"`
	Identity AgentIdentity `yaml:"identity"`
	ZFS      ZFSConfig     `yaml:"zfs"`
	Logging  LogConfig     `yaml:"logging"`
}

type AgentListen struct {
	Port int `yaml:"port"`
}

type AgentHub struct {
	URL string `yaml:"url"`
	PSK string `yaml:"psk"`
}

type AgentIdentity struct {
	AgentID       string `yaml:"agent_id"`
	AdvertiseAddr string `yaml:"advertise_addr"`
}

type ZFSConfig struct {
	ZpoolPath string `yaml:"zpool_path"`
	ZfsPath   string `yaml:"zfs_path"`
}

func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Listen:  AgentListen{Port: 9600},
		Hub:     AgentHub{URL: "http://zfs-manager:9500"},
		Logging: LogConfig{Level: "info"},
	}
}

func LoadAgentConfig(path string) (*AgentConfig, error) {
	cfg := DefaultAgentConfig()

	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parsing agent config: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading agent config %s: %w", path, err)
	}

	applyAgentEnvOverrides(&cfg)

	if err := ValidateAgentConfig(&cfg); err != nil {
		return nil, fmt.Errorf("agent config validation: %w", err)
	}

	return &cfg, nil
}

func applyAgentEnvOverrides(cfg *AgentConfig) {
	if v := os.Getenv("VIGIL_ZFS_AGENT_LISTEN_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Listen.Port = port
		}
	}
	if v := os.Getenv("VIGIL_ZFS_AGENT_HUB_URL"); v != "" {
		cfg.Hub.URL = v
	}
	if v := os.Getenv("VIGIL_ZFS_AGENT_HUB_PSK"); v != "" {
		cfg.Hub.PSK = v
	}
	if v := os.Getenv("VIGIL_ZFS_AGENT_ID"); v != "" {
		cfg.Identity.AgentID = v
	}
	if v := os.Getenv("VIGIL_ZFS_AGENT_ADVERTISE_ADDR"); v != "" {
		cfg.Identity.AdvertiseAddr = v
	}
	if v := os.Getenv("VIGIL_ZFS_AGENT_ZFS_ZPOOL_PATH"); v != "" {
		cfg.ZFS.ZpoolPath = v
	}
	if v := os.Getenv("VIGIL_ZFS_AGENT_ZFS_ZFS_PATH"); v != "" {
		cfg.ZFS.ZfsPath = v
	}
	if v := os.Getenv("VIGIL_ZFS_AGENT_LOGGING_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
}
