package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type AgentConfig struct {
	Listen     AgentListen     `yaml:"listen"`
	Hub        AgentHub        `yaml:"hub"`
	SnapRAID   SnapRAIDPaths   `yaml:"snapraid"`
	Scheduler  SchedulerConfig `yaml:"scheduler"`
	Thresholds Thresholds      `yaml:"thresholds"`
	Scrub      ScrubConfig     `yaml:"scrub"`
	Sync       SyncConfig      `yaml:"sync"`
	Hooks      HooksConfig     `yaml:"hooks"`
	Docker     DockerConfig    `yaml:"docker"`
	Logging    LogConfig       `yaml:"logging"`
}

type HooksConfig struct {
	PreSync  string `yaml:"pre_sync"`
	PostSync string `yaml:"post_sync"`
}

type DockerConfig struct {
	PauseContainers []string `yaml:"pause_containers"`
	StopContainers  []string `yaml:"stop_containers"`
}

type AgentListen struct {
	Port int `yaml:"port"`
}

type AgentHub struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

type SnapRAIDPaths struct {
	BinaryPath string `yaml:"binary_path"`
	ConfigPath string `yaml:"config_path"`
}

type SchedulerConfig struct {
	MaintenanceCron string `yaml:"maintenance_cron"`
	ScrubCron       string `yaml:"scrub_cron"`
	SmartCron       string `yaml:"smart_cron"`
	StatusCron      string `yaml:"status_cron"`
}

type Thresholds struct {
	MaxDeleted          int     `yaml:"max_deleted"`
	MaxUpdated          int     `yaml:"max_updated"`
	AddDelRatio         float64 `yaml:"add_del_ratio"`
	SmartFailProbability int    `yaml:"smart_fail_probability"`
}

type ScrubConfig struct {
	Plan             string `yaml:"plan"`
	OlderThanDays    int    `yaml:"older_than_days"`
	AutoFixBadBlocks bool   `yaml:"auto_fix_bad_blocks"`
}

type SyncConfig struct {
	PreHash bool `yaml:"pre_hash"`
}

func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Listen:   AgentListen{Port: 9400},
		Hub:      AgentHub{URL: "http://snapraid-hub:9300"},
		SnapRAID: SnapRAIDPaths{ConfigPath: "/etc/snapraid.conf"},
		Scheduler: SchedulerConfig{
			MaintenanceCron: "0 3 * * *",
			ScrubCron:       "0 4 * * 0",
			SmartCron:       "0 */6 * * *",
			StatusCron:      "*/30 * * * *",
		},
		Thresholds: Thresholds{
			MaxDeleted:           50,
			MaxUpdated:           -1,
			AddDelRatio:          -1.0,
			SmartFailProbability: 50,
		},
		Scrub: ScrubConfig{
			Plan:          "8",
			OlderThanDays: 10,
		},
		Logging: LogConfig{
			Level:      "info",
			File:       "/var/log/vigil-snapraid-agent.log",
			MaxSizeMB:  50,
			MaxBackups: 3,
		},
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
	// If file doesn't exist, continue with defaults + env overrides

	applyAgentEnvOverrides(&cfg)

	if err := ValidateAgentConfig(&cfg); err != nil {
		return nil, fmt.Errorf("agent config validation: %w", err)
	}

	return &cfg, nil
}

func applyAgentEnvOverrides(cfg *AgentConfig) {
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_LISTEN_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Listen.Port = port
		}
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_HUB_URL"); v != "" {
		cfg.Hub.URL = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_HUB_TOKEN"); v != "" {
		cfg.Hub.Token = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SNAPRAID_BINARY_PATH"); v != "" {
		cfg.SnapRAID.BinaryPath = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SNAPRAID_CONFIG_PATH"); v != "" {
		cfg.SnapRAID.ConfigPath = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SCHEDULER_MAINTENANCE_CRON"); v != "" {
		cfg.Scheduler.MaintenanceCron = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SCHEDULER_SCRUB_CRON"); v != "" {
		cfg.Scheduler.ScrubCron = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SCHEDULER_SMART_CRON"); v != "" {
		cfg.Scheduler.SmartCron = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SCHEDULER_STATUS_CRON"); v != "" {
		cfg.Scheduler.StatusCron = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_THRESHOLDS_MAX_DELETED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Thresholds.MaxDeleted = n
		}
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_THRESHOLDS_MAX_UPDATED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Thresholds.MaxUpdated = n
		}
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_THRESHOLDS_ADD_DEL_RATIO"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Thresholds.AddDelRatio = f
		}
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_THRESHOLDS_SMART_FAIL_PROBABILITY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Thresholds.SmartFailProbability = n
		}
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SCRUB_PLAN"); v != "" {
		cfg.Scrub.Plan = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SCRUB_OLDER_THAN_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Scrub.OlderThanDays = n
		}
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SCRUB_AUTO_FIX_BAD_BLOCKS"); v != "" {
		cfg.Scrub.AutoFixBadBlocks = v == "true" || v == "1"
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_SYNC_PRE_HASH"); v != "" {
		cfg.Sync.PreHash = v == "true" || v == "1"
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_HOOKS_PRE_SYNC"); v != "" {
		cfg.Hooks.PreSync = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_HOOKS_POST_SYNC"); v != "" {
		cfg.Hooks.PostSync = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_DOCKER_PAUSE_CONTAINERS"); v != "" {
		cfg.Docker.PauseContainers = strings.Split(v, ",")
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_DOCKER_STOP_CONTAINERS"); v != "" {
		cfg.Docker.StopContainers = strings.Split(v, ",")
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_LOGGING_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_LOGGING_FILE"); v != "" {
		cfg.Logging.File = v
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_LOGGING_MAX_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Logging.MaxSizeMB = n
		}
	}
	if v := os.Getenv("VIGIL_SNAPRAID_AGENT_LOGGING_MAX_BACKUPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Logging.MaxBackups = n
		}
	}
}
