package hub

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// AggregatedFrame wraps an Agent's telemetry with its identity for upstream transmission.
type AggregatedFrame struct {
	AgentID   string          `json:"agent_id"`
	Hostname  string          `json:"hostname"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// agentTelemetry is the subset of the agent's TelemetryPayload that the
// Aggregator inspects for notification triggers.
type agentTelemetry struct {
	AgentID     string           `json:"agent_id"`
	ActiveJob   *agentActiveJob  `json:"active_job,omitempty"`
	SmartStatus *agentSmartState `json:"smart_status,omitempty"`
	LastEvent   *agentEvent      `json:"last_event,omitempty"`
	DaemonInfo  *agentDaemonInfo `json:"daemon_info,omitempty"`
}

type agentDaemonInfo struct {
	SnapraidVersion string `json:"snapraid_version"`
}

type agentEvent struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type agentActiveJob struct {
	Type         string `json:"type"`
	CurrentPhase string `json:"current_phase"`
}

type agentSmartState struct {
	Disks []agentSmartDisk `json:"disks"`
}

type agentSmartDisk struct {
	DiskName           string  `json:"disk_name"`
	Device             string  `json:"device"`
	Status             string  `json:"status"`
	FailureProbability float64 `json:"failure_probability"`
}

// trackedJob records the last-known job state for an agent.
type trackedJob struct {
	Type  string
	Phase string
}

// Aggregator multiplexes inbound WebSocket telemetry from multiple Agents
// and pushes wrapped frames upstream to the Vigil Server.
type Aggregator struct {
	mu        sync.RWMutex
	registry  *Registry
	upstream  chan<- []byte
	telemetry *TelemetryClient // optional, for typed notification frames
	logger    *slog.Logger

	// Per-agent tracking for state transition detection.
	lastJobs          map[string]*trackedJob // agent_id → last known job
	lastEventIDs      map[string]string      // agent_id → last forwarded event ID
	lastGateMessages  map[string]string      // agent_id → last gate_failed message (dedup)
	smartAlerted      map[string]bool        // "agent:disk" → already alerted

	// Cached latest telemetry per agent for API serving.
	latestPayloads map[string]json.RawMessage // agent_id → raw payload
}

// NewAggregator creates a telemetry Aggregator. Upstream frames are sent
// to the provided channel for the Vigil Client to transmit.
func NewAggregator(registry *Registry, upstream chan<- []byte, logger *slog.Logger) *Aggregator {
	return &Aggregator{
		registry:        registry,
		upstream:        upstream,
		logger:          logger,
		lastJobs:        make(map[string]*trackedJob),
		lastEventIDs:    make(map[string]string),
		lastGateMessages: make(map[string]string),
		smartAlerted:    make(map[string]bool),
		latestPayloads:  make(map[string]json.RawMessage),
	}
}

// SetTelemetryClient attaches a TelemetryClient for sending typed notification
// frames directly upstream. Must be called before IngestAgentFrame.
func (a *Aggregator) SetTelemetryClient(tc *TelemetryClient) {
	a.mu.Lock()
	a.telemetry = tc
	a.mu.Unlock()
}

// IngestAgentFrame receives a raw telemetry payload from an Agent,
// wraps it with Agent metadata, and forwards it upstream.
func (a *Aggregator) IngestAgentFrame(agentID string, raw []byte) {
	entry := a.registry.Get(agentID)
	if entry == nil {
		a.logger.Warn("telemetry from unknown agent", "agent_id", agentID)
		return
	}

	a.registry.Touch(agentID)

	// Cache the raw payload for API serving.
	a.mu.Lock()
	a.latestPayloads[agentID] = json.RawMessage(raw)
	a.mu.Unlock()

	// Evaluate for notification triggers before forwarding.
	a.evaluateTelemetry(agentID, raw)

	frame := AggregatedFrame{
		AgentID:   agentID,
		Hostname:  entry.Hostname,
		Timestamp: time.Now().UTC(),
		Payload:   raw,
	}

	data, err := json.Marshal(frame)
	if err != nil {
		a.logger.Error("failed to marshal aggregated frame", "agent_id", agentID, "error", err)
		return
	}

	select {
	case a.upstream <- data:
	default:
		a.logger.Warn("upstream channel full, dropping aggregated frame", "agent_id", agentID)
	}
}

// IngestLogStream forwards a real-time log stream frame from an Agent upstream.
func (a *Aggregator) IngestLogStream(agentID string, raw []byte) {
	frame := AggregatedFrame{
		AgentID:   agentID,
		Timestamp: time.Now().UTC(),
		Payload:   raw,
	}

	data, err := json.Marshal(frame)
	if err != nil {
		a.logger.Error("failed to marshal log stream frame", "agent_id", agentID, "error", err)
		return
	}

	select {
	case a.upstream <- data:
	default:
		a.logger.Warn("upstream channel full, dropping log frame", "agent_id", agentID)
	}
}

// evaluateTelemetry inspects agent telemetry for job state transitions
// and SMART warnings, emitting notifications upstream when detected.
func (a *Aggregator) evaluateTelemetry(agentID string, raw []byte) {
	a.mu.RLock()
	tc := a.telemetry
	a.mu.RUnlock()

	if tc == nil {
		return
	}

	var t agentTelemetry
	if err := json.Unmarshal(raw, &t); err != nil {
		return
	}

	// Cache snapraid version from daemon_info into the registry.
	if t.DaemonInfo != nil && t.DaemonInfo.SnapraidVersion != "" {
		a.registry.SetSnapraidVersion(agentID, t.DaemonInfo.SnapraidVersion)
	}

	a.evaluateJobTransition(tc, agentID, t.ActiveJob)
	a.evaluateSmartStatus(tc, agentID, t.SmartStatus)
	a.evaluateAgentEvent(tc, agentID, t.LastEvent)
}

// evaluateAgentEvent forwards agent-emitted events (gate failures, pipeline
// lifecycle) as notifications upstream, deduplicating by event ID.
// Gate failures are further deduplicated by message so that repeated identical
// failures (e.g. missing content file) only notify once until resolved.
func (a *Aggregator) evaluateAgentEvent(tc *TelemetryClient, agentID string, evt *agentEvent) {
	if evt == nil || evt.ID == "" {
		return
	}

	a.mu.Lock()
	if a.lastEventIDs[agentID] == evt.ID {
		a.mu.Unlock()
		return
	}
	a.lastEventIDs[agentID] = evt.ID

	// Deduplicate repeated gate failures with the same message.
	if evt.Type == "gate_failed" {
		if a.lastGateMessages[agentID] == evt.Message {
			a.mu.Unlock()
			a.logger.Debug("suppressing duplicate gate_failed notification", "agent_id", agentID)
			return
		}
		a.lastGateMessages[agentID] = evt.Message
	}

	// Clear gate failure tracking when maintenance starts successfully,
	// so the next failure (if any) will notify again.
	if evt.Type == "maintenance_started" || evt.Type == "maintenance_complete" {
		delete(a.lastGateMessages, agentID)
	}

	a.mu.Unlock()

	a.emitNotification(tc, agentID, evt.Type, evt.Severity, evt.Message+" on "+agentID)
}

// evaluateJobTransition detects job started/completed and phase transitions.
func (a *Aggregator) evaluateJobTransition(tc *TelemetryClient, agentID string, job *agentActiveJob) {
	a.mu.Lock()
	prev := a.lastJobs[agentID]

	if job != nil && prev == nil {
		// Job started.
		a.lastJobs[agentID] = &trackedJob{Type: job.Type, Phase: job.CurrentPhase}
		a.mu.Unlock()

		a.emitNotification(tc, agentID, "job_started", "info",
			"SnapRAID "+job.Type+" started on "+agentID)
		return
	}

	if job == nil && prev != nil {
		// Job completed (disappeared from active telemetry).
		delete(a.lastJobs, agentID)
		a.mu.Unlock()

		a.emitNotification(tc, agentID, "job_complete", "info",
			"SnapRAID "+prev.Type+" completed on "+agentID)
		return
	}

	if job != nil {
		// Detect phase transition within a running job.
		prevPhase := ""
		if prev != nil {
			prevPhase = prev.Phase
		}
		a.lastJobs[agentID] = &trackedJob{Type: job.Type, Phase: job.CurrentPhase}
		a.mu.Unlock()

		if prevPhase != "" && job.CurrentPhase != "" && prevPhase != job.CurrentPhase {
			a.emitNotification(tc, agentID, "phase_complete", "info",
				"SnapRAID "+prevPhase+" completed, now running "+job.CurrentPhase+" on "+agentID)
		}
		return
	}

	a.mu.Unlock()
}

// evaluateSmartStatus checks for SMART failures and emits warnings.
func (a *Aggregator) evaluateSmartStatus(tc *TelemetryClient, agentID string, smart *agentSmartState) {
	if smart == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, disk := range smart.Disks {
		if disk.Status != "FAIL" && disk.Status != "PREFAIL" {
			continue
		}

		key := agentID + ":" + disk.Device
		if a.smartAlerted[key] {
			continue
		}
		a.smartAlerted[key] = true

		a.mu.Unlock()
		a.emitNotification(tc, agentID, "smart_warning", "warning",
			"SMART "+disk.Status+" detected on "+disk.DiskName+" ("+disk.Device+") on "+agentID)
		a.mu.Lock()
	}
}

// emitCommandFailure sends a job_failed notification when a command route fails.
func (a *Aggregator) emitCommandFailure(agentID, action string, err error) {
	a.mu.RLock()
	tc := a.telemetry
	a.mu.RUnlock()

	if tc == nil {
		return
	}

	a.emitNotification(tc, agentID, "job_failed", "critical",
		"SnapRAID "+action+" failed on "+agentID+": "+err.Error())
}

// LatestTelemetryField extracts a top-level field from the cached telemetry of
// the first online agent (or the specified agent). Returns nil if not available.
func (a *Aggregator) LatestTelemetryField(agentID, field string) json.RawMessage {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// If no agent specified, pick the first one with cached data.
	if agentID == "" {
		for id := range a.latestPayloads {
			agentID = id
			break
		}
	}

	raw, ok := a.latestPayloads[agentID]
	if !ok {
		return nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}

	return m[field]
}

func (a *Aggregator) emitNotification(tc *TelemetryClient, agentID, eventType, severity, message string) {
	n := NotificationPayload{
		EventType: eventType,
		Severity:  severity,
		Source:    "snapraid-hub",
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	if err := tc.SendNotification(n); err != nil {
		a.logger.Warn("failed to transmit notification upstream",
			"event_type", eventType,
			"agent_id", agentID,
			"error", err,
		)
		return
	}

	a.logger.Info("notification emitted",
		"event_type", eventType,
		"severity", severity,
		"agent_id", agentID,
	)
}
