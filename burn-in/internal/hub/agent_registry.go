package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DriveInfo describes a single drive attached to an agent.
type DriveInfo struct {
	Path          string `json:"path"`
	Model         string `json:"model"`
	Serial        string `json:"serial"`
	CapacityBytes int64  `json:"capacity_bytes"`
}

// AgentRegistration is the payload an agent sends when registering.
type AgentRegistration struct {
	AgentID       string      `json:"agent_id"`
	Hostname      string      `json:"hostname"`
	Arch          string      `json:"arch"`
	AdvertiseAddr string      `json:"advertise_addr"`
	Drives        []DriveInfo `json:"drives"`
}

// AgentRecord is what the registry stores for each agent.
type AgentRecord struct {
	AgentRegistration
	RegisteredAt time.Time `json:"registered_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// AgentRegistry manages the fleet of registered burn-in agents.
type AgentRegistry struct {
	mu       sync.RWMutex
	agents   map[string]*AgentRecord
	dataDir  string
	filePath string
	logger   *slog.Logger
}

// NewAgentRegistry creates a registry, loading any persisted state from dataDir.
func NewAgentRegistry(dataDir string, logger *slog.Logger) (*AgentRegistry, error) {
	r := &AgentRegistry{
		agents:   make(map[string]*AgentRecord),
		dataDir:  dataDir,
		filePath: filepath.Join(dataDir, "agents.json"),
		logger:   logger,
	}

	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating data dir %s: %w", dataDir, err)
	}

	if err := r.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading persisted agents: %w", err)
	}

	return r, nil
}

// Register adds or updates an agent in the registry.
func (r *AgentRegistry) Register(reg AgentRegistration) *AgentRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()

	existing, ok := r.agents[reg.AgentID]
	if ok {
		existing.AgentRegistration = reg
		existing.LastSeenAt = now
		r.logger.Info("agent re-registered", "agent_id", reg.AgentID, "hostname", reg.Hostname)
	} else {
		existing = &AgentRecord{
			AgentRegistration: reg,
			RegisteredAt:      now,
			LastSeenAt:        now,
		}
		r.agents[reg.AgentID] = existing
		r.logger.Info("agent registered", "agent_id", reg.AgentID, "hostname", reg.Hostname, "drives", len(reg.Drives))
	}

	if err := r.persistLocked(); err != nil {
		r.logger.Error("failed to persist agent registry", "error", err)
	}

	return existing
}

// Get returns a single agent record, or nil if not found.
func (r *AgentRegistry) Get(agentID string) *AgentRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec := r.agents[agentID]
	if rec == nil {
		return nil
	}
	cp := *rec
	return &cp
}

// List returns all registered agents.
func (r *AgentRegistry) List() []AgentRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]AgentRecord, 0, len(r.agents))
	for _, rec := range r.agents {
		out = append(out, *rec)
	}
	return out
}

// load reads persisted agents from disk.
func (r *AgentRegistry) load() error {
	data, err := os.ReadFile(r.filePath)
	if err != nil {
		return err
	}

	var records []*AgentRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("unmarshaling agents file: %w", err)
	}

	for _, rec := range records {
		r.agents[rec.AgentID] = rec
	}
	r.logger.Info("loaded persisted agents", "count", len(records))
	return nil
}

// persistLocked writes the current registry to disk. Caller must hold r.mu.
func (r *AgentRegistry) persistLocked() error {
	records := make([]*AgentRecord, 0, len(r.agents))
	for _, rec := range r.agents {
		records = append(records, rec)
	}

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}

	tmp := r.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, r.filePath)
}
