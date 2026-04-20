package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type AgentConfig struct {
	Listen     AgentListen     `yaml:"listen"`
	Hub        AgentHub        `yaml:"hub"`
	Identity   AgentIdentity   `yaml:"identity"`
	SnapRAID   SnapRAIDPaths   `yaml:"snapraid"`
	Scheduler  SchedulerConfig `yaml:"scheduler"`
	Thresholds Thresholds      `yaml:"thresholds"`
	Scrub      ScrubConfig     `yaml:"scrub"`
	Sync       SyncConfig      `yaml:"sync"`
	Timeouts   TimeoutsConfig  `yaml:"timeouts"`
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
	URL string `yaml:"url"`
	PSK string `yaml:"psk"`
}

type AgentIdentity struct {
	AgentID       string `yaml:"agent_id"`
	AdvertiseAddr string `yaml:"advertise_addr"`
}

type SnapRAIDPaths struct {
	BinaryPath string `yaml:"binary_path"`
	ConfigPath string `yaml:"config_path"`
}

type SchedulerConfig struct {
	MaintenanceCron string `yaml:"maintenance_cron"`
	ScrubCron       string `yaml:"scrub_cron"`
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

// TimeoutsConfig holds per-command timeout overrides (in minutes).
// Zero means use the built-in default.
type TimeoutsConfig struct {
	Touch   int `yaml:"touch"`   // default: 5 min
	Status  int `yaml:"status"`  // default: 5 min
	Diff    int `yaml:"diff"`    // default: 5 min
	Smart   int `yaml:"smart"`   // default: 5 min
	Sync    int `yaml:"sync"`    // default: 60 min
	Scrub   int `yaml:"scrub"`   // default: 480 min (8h)
	Fix     int `yaml:"fix"`     // default: 60 min
	Default int `yaml:"default"` // default: 10 min
}

// ForCommand returns the timeout duration for a given snapraid command.
func (t TimeoutsConfig) ForCommand(cmd string) time.Duration {
	var mins int
	switch cmd {
	case "touch":
		mins = t.Touch
	case "status":
		mins = t.Status
	case "diff":
		mins = t.Diff
	case "smart":
		mins = t.Smart
	case "sync":
		mins = t.Sync
	case "scrub":
		mins = t.Scrub
	case "fix":
		mins = t.Fix
	default:
		mins = t.Default
	}
	if mins <= 0 {
		mins = 10 // safety fallback
	}
	return time.Duration(mins) * time.Minute
}

func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Listen:   AgentListen{Port: 9400},
		Hub:      AgentHub{URL: "http://snapraid-hub:9300"},
		SnapRAID: SnapRAIDPaths{ConfigPath: "/etc/snapraid.conf"},
		Scheduler: SchedulerConfig{
			MaintenanceCron: "0 2 * * *",
			ScrubCron:       "0 6 1 * *",
			StatusCron:      "0 */6 * * *",
		},
		Thresholds: Thresholds{
			MaxDeleted:           250,
			MaxUpdated:           3000,
			AddDelRatio:          -1.0,
			SmartFailProbability: 50,
		},
		Scrub: ScrubConfig{
			Plan:             "8",
			OlderThanDays:    10,
			AutoFixBadBlocks: true,
		},
		Sync: SyncConfig{
			PreHash: true,
		},
		Timeouts: TimeoutsConfig{
			Touch:   5,
			Status:  5,
			Diff:    5,
			Smart:   5,
			Sync:    60,
			Scrub:   480,
			Fix:     60,
			Default: 10,
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

// envOverride maps an environment variable to a setter that applies it to the config.
type envOverride struct {
	key   string
	apply func(cfg *AgentConfig, v string)
}

var agentEnvOverrides = []envOverride{
	{"VIGIL_SNAPRAID_AGENT_LISTEN_PORT", func(c *AgentConfig, v string) { setInt(v, &c.Listen.Port) }},
	{"VIGIL_SNAPRAID_AGENT_HUB_URL", func(c *AgentConfig, v string) { c.Hub.URL = v }},
	{"VIGIL_SNAPRAID_AGENT_HUB_PSK", func(c *AgentConfig, v string) { c.Hub.PSK = v }},
	{"VIGIL_SNAPRAID_AGENT_ID", func(c *AgentConfig, v string) { c.Identity.AgentID = v }},
	{"VIGIL_SNAPRAID_AGENT_ADVERTISE_ADDR", func(c *AgentConfig, v string) { c.Identity.AdvertiseAddr = v }},
	{"VIGIL_SNAPRAID_AGENT_SNAPRAID_BINARY_PATH", func(c *AgentConfig, v string) { c.SnapRAID.BinaryPath = v }},
	{"VIGIL_SNAPRAID_AGENT_SNAPRAID_CONFIG_PATH", func(c *AgentConfig, v string) { c.SnapRAID.ConfigPath = v }},
	{"VIGIL_SNAPRAID_AGENT_SCHEDULER_MAINTENANCE_CRON", func(c *AgentConfig, v string) { c.Scheduler.MaintenanceCron = v }},
	{"VIGIL_SNAPRAID_AGENT_SCHEDULER_SCRUB_CRON", func(c *AgentConfig, v string) { c.Scheduler.ScrubCron = v }},
	{"VIGIL_SNAPRAID_AGENT_SCHEDULER_STATUS_CRON", func(c *AgentConfig, v string) { c.Scheduler.StatusCron = v }},
	{"VIGIL_SNAPRAID_AGENT_THRESHOLDS_MAX_DELETED", func(c *AgentConfig, v string) { setInt(v, &c.Thresholds.MaxDeleted) }},
	{"VIGIL_SNAPRAID_AGENT_THRESHOLDS_MAX_UPDATED", func(c *AgentConfig, v string) { setInt(v, &c.Thresholds.MaxUpdated) }},
	{"VIGIL_SNAPRAID_AGENT_THRESHOLDS_ADD_DEL_RATIO", func(c *AgentConfig, v string) { setFloat(v, &c.Thresholds.AddDelRatio) }},
	{"VIGIL_SNAPRAID_AGENT_THRESHOLDS_SMART_FAIL_PROBABILITY", func(c *AgentConfig, v string) { setInt(v, &c.Thresholds.SmartFailProbability) }},
	{"VIGIL_SNAPRAID_AGENT_SCRUB_PLAN", func(c *AgentConfig, v string) { c.Scrub.Plan = v }},
	{"VIGIL_SNAPRAID_AGENT_SCRUB_OLDER_THAN_DAYS", func(c *AgentConfig, v string) { setInt(v, &c.Scrub.OlderThanDays) }},
	{"VIGIL_SNAPRAID_AGENT_SCRUB_AUTO_FIX_BAD_BLOCKS", func(c *AgentConfig, v string) { c.Scrub.AutoFixBadBlocks = parseBool(v) }},
	{"VIGIL_SNAPRAID_AGENT_SYNC_PRE_HASH", func(c *AgentConfig, v string) { c.Sync.PreHash = parseBool(v) }},
	{"VIGIL_SNAPRAID_AGENT_HOOKS_PRE_SYNC", func(c *AgentConfig, v string) { c.Hooks.PreSync = v }},
	{"VIGIL_SNAPRAID_AGENT_HOOKS_POST_SYNC", func(c *AgentConfig, v string) { c.Hooks.PostSync = v }},
	{"VIGIL_SNAPRAID_AGENT_DOCKER_PAUSE_CONTAINERS", func(c *AgentConfig, v string) { c.Docker.PauseContainers = strings.Split(v, ",") }},
	{"VIGIL_SNAPRAID_AGENT_DOCKER_STOP_CONTAINERS", func(c *AgentConfig, v string) { c.Docker.StopContainers = strings.Split(v, ",") }},
	{"VIGIL_SNAPRAID_AGENT_TIMEOUTS_TOUCH", func(c *AgentConfig, v string) { setInt(v, &c.Timeouts.Touch) }},
	{"VIGIL_SNAPRAID_AGENT_TIMEOUTS_STATUS", func(c *AgentConfig, v string) { setInt(v, &c.Timeouts.Status) }},
	{"VIGIL_SNAPRAID_AGENT_TIMEOUTS_DIFF", func(c *AgentConfig, v string) { setInt(v, &c.Timeouts.Diff) }},
	{"VIGIL_SNAPRAID_AGENT_TIMEOUTS_SMART", func(c *AgentConfig, v string) { setInt(v, &c.Timeouts.Smart) }},
	{"VIGIL_SNAPRAID_AGENT_TIMEOUTS_SYNC", func(c *AgentConfig, v string) { setInt(v, &c.Timeouts.Sync) }},
	{"VIGIL_SNAPRAID_AGENT_TIMEOUTS_SCRUB", func(c *AgentConfig, v string) { setInt(v, &c.Timeouts.Scrub) }},
	{"VIGIL_SNAPRAID_AGENT_TIMEOUTS_FIX", func(c *AgentConfig, v string) { setInt(v, &c.Timeouts.Fix) }},
	{"VIGIL_SNAPRAID_AGENT_TIMEOUTS_DEFAULT", func(c *AgentConfig, v string) { setInt(v, &c.Timeouts.Default) }},
	{"VIGIL_SNAPRAID_AGENT_LOGGING_LEVEL", func(c *AgentConfig, v string) { c.Logging.Level = v }},
	{"VIGIL_SNAPRAID_AGENT_LOGGING_FILE", func(c *AgentConfig, v string) { c.Logging.File = v }},
	{"VIGIL_SNAPRAID_AGENT_LOGGING_MAX_SIZE_MB", func(c *AgentConfig, v string) { setInt(v, &c.Logging.MaxSizeMB) }},
	{"VIGIL_SNAPRAID_AGENT_LOGGING_MAX_BACKUPS", func(c *AgentConfig, v string) { setInt(v, &c.Logging.MaxBackups) }},
}

func applyAgentEnvOverrides(cfg *AgentConfig) {
	for _, o := range agentEnvOverrides {
		if v := os.Getenv(o.key); v != "" {
			o.apply(cfg, v)
		}
	}
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
