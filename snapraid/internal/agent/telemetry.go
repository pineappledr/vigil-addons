package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pineappledr/vigil-addons/snapraid/internal/engine"
)

// TelemetryPayload is the full telemetry frame transmitted to the Hub.
type TelemetryPayload struct {
	AgentID        string                     `json:"agent_id"`
	Hostname       string                     `json:"hostname"`
	Timestamp      time.Time                  `json:"timestamp"`
	ArrayStatus    *engine.StatusReport       `json:"array_status,omitempty"`
	SmartStatus    *engine.SmartReport        `json:"smart_status,omitempty"`
	DiffStatus     *engine.DiffReport         `json:"diff_status,omitempty"`
	DiskStorage    []engine.DiskStorageInfo   `json:"disk_storage,omitempty"`
	SchedulerState *SchedulerState            `json:"scheduler_state,omitempty"`
	ActiveJob      *ActiveJob                 `json:"active_job,omitempty"`
	LastEvent      *AgentEvent                `json:"last_event,omitempty"`
	DaemonInfo     DaemonInfo                 `json:"daemon_info"`
}

// AgentEvent describes a notable event that occurred on the Agent.
// The Hub Aggregator watches for new events to emit notifications.
type AgentEvent struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`      // "gate_failed", "maintenance_started", "maintenance_complete", "auto_fix"
	Severity  string    `json:"severity"`  // "info", "warning", "critical"
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// SchedulerState reflects the next/last times for each scheduled job.
type SchedulerState struct {
	NextMaintenance *time.Time `json:"next_maintenance,omitempty"`
	NextScrub       *time.Time `json:"next_scrub,omitempty"`
	NextSmartCheck  *time.Time `json:"next_smart_check,omitempty"`
	LastSyncTime    *time.Time `json:"last_sync_time,omitempty"`
	LastScrubTime   *time.Time `json:"last_scrub_time,omitempty"`
	LastSmartTime   *time.Time `json:"last_smart_time,omitempty"`
	LastSyncResult  string     `json:"last_sync_result,omitempty"`
	LastScrubResult string     `json:"last_scrub_result,omitempty"`
}

// ActiveJob describes a currently running operation.
type ActiveJob struct {
	Type            string     `json:"type"`
	Trigger         string     `json:"trigger"` // "manual" or "scheduled"
	StartedAt       *time.Time `json:"started_at,omitempty"`
	ProgressPercent int        `json:"progress_percent"`
	CurrentPhase    string     `json:"current_phase"`
}

// DaemonInfo holds static agent metadata.
type DaemonInfo struct {
	Version         string `json:"version"`
	Uptime          string `json:"uptime"`
	HubConnected    bool   `json:"hub_connected"`
	SnapraidVersion string `json:"snapraid_version"`
}

// Collector aggregates engine data into telemetry payloads.
type Collector struct {
	mu             sync.RWMutex
	agentID        string
	hostname       string
	startTime      time.Time
	version        string
	hubConnected   bool
	arrayStatus    *engine.StatusReport
	smartStatus    *engine.SmartReport
	diffStatus     *engine.DiffReport
	diskStorage    []engine.DiskStorageInfo
	schedulerState *SchedulerState
	activeJob       *ActiveJob
	lastEvent       *AgentEvent
	snapraidVersion string
	logger          *slog.Logger
}

// NewCollector creates a telemetry Collector with the given identity.
func NewCollector(agentID, hostname, version string, logger *slog.Logger) *Collector {
	return &Collector{
		agentID:   agentID,
		hostname:  hostname,
		startTime: time.Now(),
		version:   version,
		logger:    logger,
	}
}

func (c *Collector) SetArrayStatus(r *engine.StatusReport) {
	// Strip raw output to avoid bloating telemetry payloads.
	clean := *r
	clean.Output = ""
	c.mu.Lock()
	c.arrayStatus = &clean
	c.mu.Unlock()
}
func (c *Collector) SetSmartStatus(r *engine.SmartReport) {
	clean := *r
	clean.Output = ""
	c.mu.Lock()
	c.smartStatus = &clean
	c.mu.Unlock()
}
func (c *Collector) SetDiffStatus(r *engine.DiffReport) {
	clean := *r
	clean.Output = ""
	c.mu.Lock()
	c.diffStatus = &clean
	c.mu.Unlock()
}
func (c *Collector) SetDiskStorage(d []engine.DiskStorageInfo) { c.mu.Lock(); c.diskStorage = d; c.mu.Unlock() }
func (c *Collector) SetSchedulerState(s *SchedulerState)      { c.mu.Lock(); c.schedulerState = s; c.mu.Unlock() }
func (c *Collector) SetActiveJob(j *ActiveJob)               { c.mu.Lock(); c.activeJob = j; c.mu.Unlock() }
func (c *Collector) ClearActiveJob()                         { c.mu.Lock(); c.activeJob = nil; c.mu.Unlock() }

// TrackJob implements scheduler.JobTracker — sets the active job visible on the dashboard.
func (c *Collector) TrackJob(jobType, trigger, phase string) {
	now := time.Now().UTC()
	c.SetActiveJob(&ActiveJob{
		Type:         jobType,
		Trigger:      trigger,
		StartedAt:    &now,
		CurrentPhase: phase,
	})
}

// UpdateProgress sets the progress percentage on the active job.
func (c *Collector) UpdateProgress(pct int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeJob != nil {
		c.activeJob.ProgressPercent = pct
	}
}

// ClearJob implements scheduler.JobTracker — clears the active job after completion.
func (c *Collector) ClearJob() {
	c.ClearActiveJob()
}
func (c *Collector) SetHubConnected(v bool)                  { c.mu.Lock(); c.hubConnected = v; c.mu.Unlock() }
func (c *Collector) SetSnapraidVersion(v string)             { c.mu.Lock(); c.snapraidVersion = v; c.mu.Unlock() }

// GetArrayStatus returns the cached array status, or nil if not yet populated.
func (c *Collector) GetArrayStatus() *engine.StatusReport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.arrayStatus
}

// GetSmartStatus returns the cached SMART status, or nil if not yet populated.
func (c *Collector) GetSmartStatus() *engine.SmartReport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.smartStatus
}

// GetDiskStorage returns the cached disk storage info.
func (c *Collector) GetDiskStorage() []engine.DiskStorageInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.diskStorage
}

// GetActiveJob returns the cached active job, or nil if none.
func (c *Collector) GetActiveJob() *ActiveJob {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.activeJob
}

// SetLastEvent records a notable event. The event is included in the next
// telemetry frame and automatically cleared after one transmission.
func (c *Collector) SetLastEvent(e *AgentEvent) { c.mu.Lock(); c.lastEvent = e; c.mu.Unlock() }

// EmitEvent implements the scheduler.EventEmitter interface.
func (c *Collector) EmitEvent(eventType, severity, message string) {
	c.SetLastEvent(&AgentEvent{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Type:      eventType,
		Severity:  severity,
		Message:   message,
		Timestamp: time.Now().UTC(),
	})
}

// Build assembles the current telemetry state into a payload.
// Uses a write lock because it clears LastEvent after reading.
func (c *Collector) Build() *TelemetryPayload {
	c.mu.Lock()
	defer c.mu.Unlock()

	evt := c.lastEvent
	// Clear event after reading so it's only transmitted once.
	c.lastEvent = nil

	return &TelemetryPayload{
		AgentID:        c.agentID,
		Hostname:       c.hostname,
		Timestamp:      time.Now().UTC(),
		ArrayStatus:    c.arrayStatus,
		SmartStatus:    c.smartStatus,
		DiffStatus:     c.diffStatus,
		DiskStorage:    c.diskStorage,
		SchedulerState: c.schedulerState,
		ActiveJob:      c.activeJob,
		LastEvent:      evt,
		DaemonInfo: DaemonInfo{
			Version:         c.version,
			Uptime:          time.Since(c.startTime).Truncate(time.Second).String(),
			HubConnected:    c.hubConnected,
			SnapraidVersion: c.snapraidVersion,
		},
	}
}

// MarshalPayload serializes the current telemetry to JSON.
func (c *Collector) MarshalPayload() ([]byte, error) {
	return json.Marshal(c.Build())
}

// WSMessage is a typed WebSocket message envelope.
type WSMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// TelemetryLoop periodically builds and sends telemetry to the provided channel.
// It stops when ctx is cancelled.
func (c *Collector) TelemetryLoop(ctx context.Context, interval time.Duration, send chan<- []byte) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := c.MarshalPayload()
			if err != nil {
				c.logger.Error("failed to marshal telemetry", "error", err)
				continue
			}
			msg, _ := json.Marshal(WSMessage{
				Type:    "telemetry",
				Payload: data,
			})
			select {
			case send <- msg:
			default:
				c.logger.Warn("telemetry send channel full, dropping frame")
			}
		}
	}
}
