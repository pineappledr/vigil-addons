package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// AgentEntry represents a registered Agent in the Hub's registry.
type AgentEntry struct {
	ID           string    `json:"agent_id"`
	Hostname     string    `json:"hostname"`
	Address      string    `json:"address"`
	Version      string    `json:"version"`
	RegisteredAt time.Time `json:"registered_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// Registry tracks connected Agents and persists state to a JSON file.
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

// Register adds or updates an Agent in the registry and persists to disk.
func (r *Registry) Register(entry AgentEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if existing, ok := r.agents[entry.ID]; ok {
		existing.Address = entry.Address
		existing.Version = entry.Version
		existing.Hostname = entry.Hostname
		existing.LastSeenAt = now
	} else {
		entry.RegisteredAt = now
		entry.LastSeenAt = now
		r.agents[entry.ID] = &entry
	}

	return r.save()
}

// Get returns an Agent entry by ID, or nil if not found.
func (r *Registry) Get(id string) *AgentEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.agents[id]
	if !ok {
		return nil
	}
	copy := *entry
	return &copy
}

// List returns all registered Agents.
func (r *Registry) List() []AgentEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]AgentEntry, 0, len(r.agents))
	for _, e := range r.agents {
		entries = append(entries, *e)
	}
	return entries
}

const agentOnlineThreshold = 2 * time.Minute

// AgentView is the API representation with a computed status field.
type AgentView struct {
	AgentEntry
	Status string `json:"status"` // "online", "offline"
}

// ListViews returns all agents with a computed online/offline status.
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

// Delete removes an Agent from the registry. Returns true if the agent existed.
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

// Touch updates the LastSeenAt timestamp for an Agent.
func (r *Registry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.agents[id]; ok {
		e.LastSeenAt = time.Now().UTC()
	}
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
		r.agents[entries[i].ID] = &entries[i]
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
		return fmt.Errorf("marshal registry: %w", err)
	}

	return os.WriteFile(r.filePath, data, 0644)
}
