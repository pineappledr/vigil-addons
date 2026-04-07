package manager

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

const agentOnlineThreshold = 2 * time.Minute

// AgentEntry represents a registered ZFS agent.
type AgentEntry struct {
	ID            string    `json:"agent_id"`
	Hostname      string    `json:"hostname"`
	Arch          string    `json:"arch"`
	Address       string    `json:"address"`
	Version       string    `json:"version"`
	RegisteredAt  time.Time `json:"registered_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
}

// AgentView adds a computed online/offline status.
type AgentView struct {
	AgentEntry
	Status string `json:"status"`
}

// Registry tracks connected agents and persists state to a JSON file.
type Registry struct {
	mu       sync.RWMutex
	agents   map[string]*AgentEntry
	filePath string
}

// NewRegistry creates or loads a Registry from the given file path.
func NewRegistry(filePath string) (*Registry, error) {
	r := &Registry{
		agents:   make(map[string]*AgentEntry),
		filePath: filePath,
	}
	if err := r.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load registry: %w", err)
	}
	return r, nil
}

// Register adds or updates an agent.
func (r *Registry) Register(entry AgentEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if existing, ok := r.agents[entry.ID]; ok {
		existing.Hostname = entry.Hostname
		existing.Arch = entry.Arch
		existing.Address = entry.Address
		existing.Version = entry.Version
		existing.LastSeenAt = now
	} else {
		entry.RegisteredAt = now
		entry.LastSeenAt = now
		r.agents[entry.ID] = &entry
	}
	return r.save()
}

// Touch updates LastSeenAt for an agent.
func (r *Registry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.agents[id]; ok {
		e.LastSeenAt = time.Now().UTC()
	}
}

// Delete removes an agent. Returns true if it existed.
func (r *Registry) Delete(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[id]; !ok {
		return false
	}
	delete(r.agents, id)
	r.save()
	return true
}

// Get returns an agent by ID, or nil.
func (r *Registry) Get(id string) *AgentEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.agents[id]
	if !ok {
		return nil
	}
	copy := *e
	return &copy
}

// ListViews returns all agents with computed online/offline status.
func (r *Registry) ListViews() []AgentView {
	r.mu.RLock()
	defer r.mu.RUnlock()

	views := make([]AgentView, 0, len(r.agents))
	for _, e := range r.agents {
		status := "offline"
		if time.Since(e.LastSeenAt) < agentOnlineThreshold {
			status = "online"
		}
		views = append(views, AgentView{AgentEntry: *e, Status: status})
	}
	return views
}

func (r *Registry) load() error {
	data, err := os.ReadFile(r.filePath)
	if err != nil {
		return err
	}
	var entries []AgentEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse registry: %w", err)
	}
	for i := range entries {
		if entries[i].ID != "" {
			r.agents[entries[i].ID] = &entries[i]
		}
	}
	return nil
}

func (r *Registry) save() error {
	entries := make([]AgentEntry, 0, len(r.agents))
	for _, e := range r.agents {
		entries = append(entries, *e)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.filePath, data, 0644)
}
