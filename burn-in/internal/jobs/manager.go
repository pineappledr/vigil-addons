package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/drive"
)

// Valid command values for job dispatch.
const (
	CommandBurnin  = "burnin"
	CommandPreclear = "preclear"
	CommandFull    = "full"
)

// JobStatus represents the current state of a tracked job.
type JobStatus string

const (
	StatusPending   JobStatus = "pending"
	StatusRunning   JobStatus = "running"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

// JobRecord holds the metadata and state for a single managed job.
type JobRecord struct {
	JobID         string          `json:"job_id"`
	Command       string          `json:"command"`
	Target        string          `json:"target"`
	DevicePath    string          `json:"device_path"`
	Status        JobStatus       `json:"status"`
	Phase         string          `json:"phase"`
	Params        json.RawMessage `json:"params,omitempty"`
	FailReason    string          `json:"fail_reason,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
	BurninPassed  *bool           `json:"burnin_passed,omitempty"`
}

// JobCommand is the inbound command payload from the hub.
type JobCommand struct {
	AgentID string          `json:"agent_id"`
	Command string          `json:"command"`
	Target  string          `json:"target"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// OnJobLifecycle is called by the manager when a job starts or finishes,
// allowing the agent API layer to register/unregister cancel functions.
type OnJobLifecycle interface {
	RegisterJob(jobID string, cancel func())
	UnregisterJob(jobID string)
}

// JobManager dispatches, tracks, and manages burn-in and pre-clear jobs.
type JobManager struct {
	sink      TelemetrySink
	persist   *JobPersistence
	lifecycle OnJobLifecycle
	logger    *slog.Logger

	mu         sync.Mutex
	jobs       map[string]*managedJob
	driveLocks map[string]string // devicePath → jobID
}

// managedJob wraps a JobRecord with its runtime cancel function.
type managedJob struct {
	record JobRecord
	cancel context.CancelFunc
}

// NewJobManager creates the job manager.
// persist may be nil to disable state persistence.
func NewJobManager(sink TelemetrySink, persist *JobPersistence, lifecycle OnJobLifecycle, logger *slog.Logger) *JobManager {
	return &JobManager{
		sink:       sink,
		persist:    persist,
		lifecycle:  lifecycle,
		logger:     logger,
		jobs:       make(map[string]*managedJob),
		driveLocks: make(map[string]string),
	}
}

// StartJob validates and dispatches a job command to the correct orchestrator.
// It returns the assigned job ID immediately; the job runs in a background goroutine.
func (m *JobManager) StartJob(cmd JobCommand) (string, error) {
	switch cmd.Command {
	case CommandBurnin, CommandPreclear, CommandFull:
	default:
		return "", fmt.Errorf("unknown command: %q", cmd.Command)
	}

	if cmd.Target == "" {
		return "", fmt.Errorf("target device is required")
	}

	// Resolve the device path to get the canonical block device.
	driveInfo, err := drive.ResolveDrive(cmd.Target)
	if err != nil {
		return "", fmt.Errorf("resolving target device: %w", err)
	}
	devicePath := driveInfo.Path

	jobID := generateJobID(cmd.Command, devicePath)

	m.mu.Lock()

	// Conflict detection: ensure the drive is not locked by another active job.
	if existingJobID, locked := m.driveLocks[devicePath]; locked {
		m.mu.Unlock()
		return "", fmt.Errorf("device %s is locked by active job %s", devicePath, existingJobID)
	}

	record := JobRecord{
		JobID:      jobID,
		Command:    cmd.Command,
		Target:     cmd.Target,
		DevicePath: devicePath,
		Status:     StatusRunning,
		Phase:      PhasePreflight,
		Params:     cmd.Params,
		StartedAt:  time.Now().UTC(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	mj := &managedJob{record: record, cancel: cancel}

	m.jobs[jobID] = mj
	m.driveLocks[devicePath] = jobID
	m.mu.Unlock()

	// Register with the agent API for abort handling.
	if m.lifecycle != nil {
		m.lifecycle.RegisterJob(jobID, cancel)
	}

	m.saveState(mj)
	m.logger.Info("job started", "job_id", jobID, "command", cmd.Command, "device", devicePath)

	// Dispatch the job in a background goroutine.
	go m.runJob(ctx, mj)

	return jobID, nil
}

// CancelJob triggers cancellation for the specified job.
func (m *JobManager) CancelJob(jobID string) error {
	m.mu.Lock()
	mj, ok := m.jobs[jobID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("job %q not found or already completed", jobID)
	}

	m.logger.Info("cancelling job", "job_id", jobID)
	mj.cancel()

	return nil
}

// GetJob returns the current record for a job.
func (m *JobManager) GetJob(jobID string) (*JobRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	mj, ok := m.jobs[jobID]
	if !ok {
		return nil, false
	}
	rec := mj.record
	return &rec, true
}

// ListJobs returns all tracked job records.
func (m *JobManager) ListJobs() []JobRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	records := make([]JobRecord, 0, len(m.jobs))
	for _, mj := range m.jobs {
		records = append(records, mj.record)
	}
	return records
}

// runJob dispatches to the correct orchestrator based on the command.
func (m *JobManager) runJob(ctx context.Context, mj *managedJob) {
	rec := &mj.record

	defer m.finishJob(mj)

	switch rec.Command {
	case CommandBurnin:
		m.runBurninJob(ctx, mj)
	case CommandPreclear:
		m.runPreclearJob(ctx, mj)
	case CommandFull:
		m.runFullJob(ctx, mj)
	}
}

func (m *JobManager) runBurninJob(ctx context.Context, mj *managedJob) {
	rec := &mj.record
	params := parseBurninParams(rec.Params)

	result, err := RunBurnin(ctx, rec.JobID, rec.DevicePath, params, m.sink, m.logger)
	if err != nil {
		if ctx.Err() != nil {
			rec.Status = StatusCancelled
			rec.FailReason = "job cancelled"
		} else {
			rec.Status = StatusFailed
			rec.FailReason = err.Error()
		}
		return
	}

	if result.Passed {
		rec.Status = StatusCompleted
	} else {
		rec.Status = StatusFailed
		rec.FailReason = result.FailReason
	}
}

func (m *JobManager) runPreclearJob(ctx context.Context, mj *managedJob) {
	rec := &mj.record
	params := parsePreclearParams(rec.Params)

	result, err := RunPreclear(ctx, rec.JobID, rec.DevicePath, params, m.sink, m.logger)
	if err != nil {
		if ctx.Err() != nil {
			rec.Status = StatusCancelled
			rec.FailReason = "job cancelled"
		} else {
			rec.Status = StatusFailed
			rec.FailReason = err.Error()
		}
		return
	}

	if result.Passed {
		rec.Status = StatusCompleted
	} else {
		rec.Status = StatusFailed
		rec.FailReason = result.FailReason
	}
}

// runFullJob runs the burn-in pipeline first; if it passes, it automatically
// transitions into the pre-clear pipeline using the same job ID.
func (m *JobManager) runFullJob(ctx context.Context, mj *managedJob) {
	rec := &mj.record
	burninParams := parseBurninParams(rec.Params)

	// Phase A: Burn-in.
	rec.Phase = "BURNIN"
	m.saveState(mj)

	burninResult, err := RunBurnin(ctx, rec.JobID, rec.DevicePath, burninParams, m.sink, m.logger)
	if err != nil {
		if ctx.Err() != nil {
			rec.Status = StatusCancelled
			rec.FailReason = "job cancelled"
		} else {
			rec.Status = StatusFailed
			rec.FailReason = err.Error()
		}
		return
	}

	if !burninResult.Passed {
		rec.Status = StatusFailed
		rec.FailReason = "burn-in failed: " + burninResult.FailReason
		return
	}

	passed := true
	rec.BurninPassed = &passed
	m.logger.Info("burn-in passed, transitioning to pre-clear", "job_id", rec.JobID)

	// Emit BurninPassed telemetry event.
	if m.sink != nil {
		m.sink.SendLog(rec.JobID, SeverityInfo, "burn-in passed, starting pre-clear phase")
	}

	if ctx.Err() != nil {
		rec.Status = StatusCancelled
		rec.FailReason = "job cancelled between phases"
		return
	}

	// Phase B: Pre-clear.
	rec.Phase = "PRECLEAR"
	m.saveState(mj)

	preclearParams := parsePreclearParams(rec.Params)

	preclearResult, err := RunPreclear(ctx, rec.JobID, rec.DevicePath, preclearParams, m.sink, m.logger)
	if err != nil {
		if ctx.Err() != nil {
			rec.Status = StatusCancelled
			rec.FailReason = "job cancelled"
		} else {
			rec.Status = StatusFailed
			rec.FailReason = err.Error()
		}
		return
	}

	if preclearResult.Passed {
		rec.Status = StatusCompleted
	} else {
		rec.Status = StatusFailed
		rec.FailReason = "pre-clear failed: " + preclearResult.FailReason
	}
}

// finishJob releases the drive lock, unregisters from the API, persists
// final state, and removes from active tracking.
func (m *JobManager) finishJob(mj *managedJob) {
	rec := &mj.record
	now := time.Now().UTC()
	rec.CompletedAt = &now
	rec.Phase = PhaseComplete

	m.saveState(mj)

	m.mu.Lock()
	delete(m.driveLocks, rec.DevicePath)
	delete(m.jobs, rec.JobID)
	m.mu.Unlock()

	if m.lifecycle != nil {
		m.lifecycle.UnregisterJob(rec.JobID)
	}

	m.logger.Info("job finished",
		"job_id", rec.JobID,
		"status", rec.Status,
		"fail_reason", rec.FailReason,
	)
}

// UpdatePhase updates the current phase of a job and persists the change.
// This can be called externally (e.g., from orchestrators via hooks).
func (m *JobManager) UpdatePhase(jobID, phase string) {
	m.mu.Lock()
	mj, ok := m.jobs[jobID]
	if ok {
		mj.record.Phase = phase
	}
	m.mu.Unlock()

	if ok {
		m.saveState(mj)
	}
}

func (m *JobManager) saveState(mj *managedJob) {
	if m.persist == nil {
		return
	}
	if err := m.persist.SaveState(mj.record); err != nil {
		m.logger.Warn("failed to persist job state", "job_id", mj.record.JobID, "error", err)
	}
}

// generateJobID creates a deterministic job ID from command and device path.
func generateJobID(command, devicePath string) string {
	ts := time.Now().UTC().Format("20060102T150405")
	devName := drive.SafeDeviceName(devicePath)
	return fmt.Sprintf("%s-%s-%s", command, devName, ts)
}

// parseBurninParams extracts BurninParams from raw JSON, applying defaults.
func parseBurninParams(raw json.RawMessage) BurninParams {
	params := BurninParams{
		BlockSize:        4096,
		ConcurrentBlocks: 65536,
		AbortOnError:     true,
		LogDir:           "/var/lib/vigil-agent/logs",
	}
	if len(raw) > 0 {
		json.Unmarshal(raw, &params)
	}
	return params
}

// parsePreclearParams extracts PreclearParams from raw JSON, applying defaults.
func parsePreclearParams(raw json.RawMessage) PreclearParams {
	params := PreclearParams{
		ReservedPct: 1,
		LogDir:      "/var/lib/vigil-agent/logs",
	}
	if len(raw) > 0 {
		json.Unmarshal(raw, &params)
	}
	return params
}
