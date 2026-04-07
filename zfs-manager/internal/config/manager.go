package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type ManagerConfig struct {
	Listen  ManagerListen `yaml:"listen"`
	Vigil   ManagerVigil  `yaml:"vigil"`
	Data    ManagerData   `yaml:"data"`
	Logging LogConfig     `yaml:"logging"`
}

type ManagerListen struct {
	Port int `yaml:"port"`
}

type ManagerVigil struct {
	ServerURL string `yaml:"server_url"`
	Token     string `yaml:"token"`
}

type ManagerData struct {
	RegistryPath string `yaml:"registry_path"`
}

func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		Listen:  ManagerListen{Port: 9500},
		Vigil:   ManagerVigil{ServerURL: "http://vigil.local:9080"},
		Data:    ManagerData{RegistryPath: "/data/agents.json"},
		Logging: LogConfig{Level: "info"},
	}
}

func LoadManagerConfig(path string) (*ManagerConfig, error) {
	cfg := DefaultManagerConfig()

	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parsing manager config: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading manager config %s: %w", path, err)
	}

	applyManagerEnvOverrides(&cfg)

	if err := ValidateManagerConfig(&cfg); err != nil {
		return nil, fmt.Errorf("manager config validation: %w", err)
	}

	return &cfg, nil
}

func applyManagerEnvOverrides(cfg *ManagerConfig) {
	if v := os.Getenv("VIGIL_ZFS_MANAGER_LISTEN_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Listen.Port = port
		}
	}
	if v := os.Getenv("VIGIL_ZFS_MANAGER_VIGIL_SERVER_URL"); v != "" {
		cfg.Vigil.ServerURL = v
	}
	if v := os.Getenv("VIGIL_ZFS_MANAGER_VIGIL_TOKEN"); v != "" {
		cfg.Vigil.Token = v
	}
	if v := os.Getenv("VIGIL_ZFS_MANAGER_DATA_REGISTRY_PATH"); v != "" {
		cfg.Data.RegistryPath = v
	}
	if v := os.Getenv("VIGIL_ZFS_MANAGER_LOGGING_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
}
