package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HubConfig holds all configuration for the burn-in hub.
type HubConfig struct {
	Vigil  VigilConfig  `json:"vigil"`
	Hub    HubServer    `json:"hub"`
	Alerts AlertConfig  `json:"alerts"`
}

type VigilConfig struct {
	URL          string `json:"url"`
	AgentToken   string `json:"agent_token"`
	ServerPubkey string `json:"server_pubkey"`
}

type HubServer struct {
	Listen            string        `json:"listen"`
	AdvertiseURL      string        `json:"advertise_url"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
	AgentPSK          string        `json:"agent_psk"`
	DataDir           string        `json:"data_dir"`
}

type AlertConfig struct {
	TempWarningC  int `json:"temp_warning_c"`
	TempCriticalC int `json:"temp_critical_c"`
}

// LoadHubConfig loads hub configuration from environment variables.
// Environment variables take the form BURNIN_<SECTION>_<KEY>.
func LoadHubConfig() (*HubConfig, error) {
	cfg := &HubConfig{
		Vigil: VigilConfig{
			URL: "http://localhost:8080",
		},
		Hub: HubServer{
			Listen:            ":9100",
			HeartbeatInterval: 30 * time.Second,
			DataDir:           "/var/lib/vigil-hub",
		},
		Alerts: AlertConfig{
			TempWarningC:  45,
			TempCriticalC: 55,
		},
	}

	// Load from config file if BURNIN_CONFIG_FILE is set.
	if path := os.Getenv("BURNIN_CONFIG_FILE"); path != "" {
		if err := loadFromFile(cfg, path); err != nil {
			return nil, fmt.Errorf("loading config file: %w", err)
		}
	}

	// Environment variables override file values.
	envStr("VIGIL_URL", &cfg.Vigil.URL)
	envStr("VIGIL_AGENT_TOKEN", &cfg.Vigil.AgentToken) // legacy
	envStr("VIGIL_TOKEN", &cfg.Vigil.AgentToken)
	envStr("VIGIL_SERVER_PUBKEY", &cfg.Vigil.ServerPubkey)
	envStr("BURNIN_HUB_LISTEN", &cfg.Hub.Listen)
	envStr("BURNIN_HUB_ADVERTISE_URL", &cfg.Hub.AdvertiseURL)
	envStr("BURNIN_HUB_AGENT_PSK", &cfg.Hub.AgentPSK)
	envStr("BURNIN_HUB_DATA_DIR", &cfg.Hub.DataDir)
	envInt("BURNIN_ALERTS_TEMP_WARNING_C", &cfg.Alerts.TempWarningC)
	envInt("BURNIN_ALERTS_TEMP_CRITICAL_C", &cfg.Alerts.TempCriticalC)

	if err := envDuration("BURNIN_HUB_HEARTBEAT_INTERVAL", &cfg.Hub.HeartbeatInterval); err != nil {
		return nil, err
	}

	// Auto-generate AgentPSK if not provided.
	if cfg.Hub.AgentPSK == "" {
		psk, err := loadOrGeneratePSK(cfg.Hub.DataDir)
		if err != nil {
			return nil, fmt.Errorf("auto-generating agent PSK: %w", err)
		}
		cfg.Hub.AgentPSK = psk
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

func loadFromFile(cfg *HubConfig, path string) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, cfg)
}

func (c *HubConfig) validate() error {
	var missing []string

	if c.Vigil.URL == "" {
		missing = append(missing, "vigil.url")
	}
	if c.Hub.Listen == "" {
		missing = append(missing, "hub.listen")
	}

	if len(missing) > 0 {
		return fmt.Errorf("required config missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// loadOrGeneratePSK loads an existing PSK from {dataDir}/hub.psk, or generates
// a new 32-byte hex-encoded key and persists it for future restarts.
func loadOrGeneratePSK(dataDir string) (string, error) {
	pskPath := filepath.Join(dataDir, "hub.psk")

	// Try to load existing PSK.
	data, err := os.ReadFile(filepath.Clean(pskPath))
	if err == nil {
		psk := strings.TrimSpace(string(data))
		if psk != "" {
			return psk, nil
		}
	}

	// Generate a new 32-byte random PSK.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating random PSK: %w", err)
	}
	psk := hex.EncodeToString(buf)

	// Ensure the data directory exists.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("creating data directory: %w", err)
	}

	// Write the PSK file.
	if err := os.WriteFile(pskPath, []byte(psk+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing PSK file: %w", err)
	}

	return psk, nil
}

