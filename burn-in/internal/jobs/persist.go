package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const defaultJobStatePath = "/var/lib/vigil-agent/jobs.json"

// JobPersistence handles saving and loading job state to/from disk.
type JobPersistence struct {
	path   string
	mu     sync.Mutex
	states map[string]JobRecord
}

// NewJobPersistence creates a persistence handler. If path is empty, the
// default path is used.
func NewJobPersistence(path string) *JobPersistence {
	if path == "" {
		path = defaultJobStatePath
	}
	return &JobPersistence{
		path:   filepath.Clean(path),
		states: make(map[string]JobRecord),
	}
}

// SaveState persists the current state of a job. It is called on every
// major phase transition to enable crash recovery.
func (p *JobPersistence) SaveState(record JobRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.states[record.JobID] = record
	return p.writeToDisk()
}

// RemoveState removes a job from the persisted state file.
// Called when a job is fully complete and no longer needs recovery.
func (p *JobPersistence) RemoveState(jobID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.states, jobID)
	return p.writeToDisk()
}

// LoadState reads the persisted state file from disk and returns all
// incomplete job records. Called on agent startup for crash recovery.
func (p *JobPersistence) LoadState() ([]JobRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading job state file: %w", err)
	}

	var records []JobRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("parsing job state file: %w", err)
	}

	// Populate the in-memory map and filter for incomplete jobs.
	var incomplete []JobRecord
	for _, rec := range records {
		p.states[rec.JobID] = rec
		if rec.Status == StatusRunning || rec.Status == StatusPending {
			incomplete = append(incomplete, rec)
		}
	}

	return incomplete, nil
}

// AllRecords returns every persisted job record regardless of status.
func (p *JobPersistence) AllRecords() []JobRecord {
	p.mu.Lock()
	defer p.mu.Unlock()

	records := make([]JobRecord, 0, len(p.states))
	for _, rec := range p.states {
		records = append(records, rec)
	}
	return records
}

// writeToDisk atomically writes all current job states to the state file.
func (p *JobPersistence) writeToDisk() error {
	records := make([]JobRecord, 0, len(p.states))
	for _, rec := range p.states {
		records = append(records, rec)
	}

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling job state: %w", err)
	}

	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	// Atomic write: write to temp file, then rename.
	tmpPath := p.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o640); err != nil {
		return fmt.Errorf("writing temp state file: %w", err)
	}

	if err := os.Rename(tmpPath, p.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming state file: %w", err)
	}

	return nil
}
