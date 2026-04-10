package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type HubConfig struct {
	Listen  HubListen  `yaml:"listen"`
	Vigil   HubVigil   `yaml:"vigil"`
	Data    HubData    `yaml:"data"`
	Logging LogConfig  `yaml:"logging"`
}

type HubListen struct {
	Port int `yaml:"port"`
}

type HubVigil struct {
	ServerURL string `yaml:"server_url"`
	Token     string `yaml:"token"`
}

type HubData struct {
	RegistryPath string `yaml:"registry_path"`
}

func DefaultHubConfig() HubConfig {
	return HubConfig{
		Listen:  HubListen{Port: 9300},
		Vigil:   HubVigil{ServerURL: "http://vigil.local:9080"},
		Data:    HubData{RegistryPath: "/data/agents.json"},
		Logging: LogConfig{Level: "info"},
	}
}

func LoadHubConfig(path string) (*HubConfig, error) {
	cfg := DefaultHubConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading hub config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing hub config: %w", err)
	}

	applyHubEnvOverrides(&cfg)

	if err := ValidateHubConfig(&cfg); err != nil {
		return nil, fmt.Errorf("hub config validation: %w", err)
	}

	return &cfg, nil
}

func applyHubEnvOverrides(cfg *HubConfig) {
	if v := os.Getenv("VIGIL_SNAPRAID_HUB_LISTEN_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Listen.Port = port
		}
	}
	if v := os.Getenv("VIGIL_SNAPRAID_HUB_VIGIL_SERVER_URL"); v != "" { // legacy
		cfg.Vigil.ServerURL = v
	}
	if v := os.Getenv("VIGIL_URL"); v != "" {
		cfg.Vigil.ServerURL = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_HUB_VIGIL_TOKEN"); v != "" { // legacy
		cfg.Vigil.Token = v
	}
	if v := os.Getenv("VIGIL_TOKEN"); v != "" {
		cfg.Vigil.Token = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_HUB_DATA_REGISTRY_PATH"); v != "" {
		cfg.Data.RegistryPath = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_HUB_LOGGING_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
}
