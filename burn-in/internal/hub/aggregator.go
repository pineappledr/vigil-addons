package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pineappledr/vigil-addons/shared/addonutil"
	"github.com/pineappledr/vigil-addons/shared/vigilclient"
)

const maxMessageSize = 64 * 1024 // 64 KB per Vigil spec

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// AlertThresholds holds temperature thresholds for notification generation.
type AlertThresholds struct {
	TempWarningC  int
	TempCriticalC int
}

// agentConn tracks a single agent's WebSocket connection.
type agentConn struct {
	agentID string
	conn    *websocket.Conn
}

// StoredLog is a log entry retained in the hub's ring buffer for historical queries.
type StoredLog struct {
	Level     string `json:"level"`
	Message   string `json:"message"`
	Source    string `json:"source"`
	JobID     string `json:"job_id,omitempty"`
	Timestamp string `json:"timestamp"`
}

const maxStoredLogs = 10000

// StoredChartPoint is a single data point retained in the hub's chart ring
// buffer for historical chart queries (e.g., drive temperature over time).
type StoredChartPoint struct {
	ComponentID string  `json:"component_id"`
	Key         string  `json:"key"`
	Value       float64 `json:"value"`
	Source      string  `json:"source,omitempty"`
	Timestamp   string  `json:"timestamp"`
}

const maxStoredChartPoints = 2000

// Aggregator accepts telemetry WebSocket connections from agents,
// evaluates frames for notification triggers, and multiplexes
// everything into the single upstream TelemetryClient.
type Aggregator struct {
	upstream   *vigilclient.TelemetryClient
	registry   *AgentRegistry
	psk        string
	thresholds AlertThresholds
	logger     *slog.Logger

	mu    sync.Mutex
	conns map[string]*agentConn

	logMu   sync.Mutex
	logRing []StoredLog

	chartMu   sync.Mutex
	chartRing []StoredChartPoint

	// progressMu guards activeProgress.
	progressMu     sync.Mutex
	activeProgress map[string]ProgressPayload // key: jobID → latest progress state

	// smartMu guards smartDeltas.
	smartMu     sync.Mutex
	smartDeltas map[string]json.RawMessage // key: jobID → latest enriched deltas JSON

	// lastPhase tracks the most recent progress phase per job so that
	// phase transitions can be stored as synthetic log entries in the
	// ring buffer, making them available to historical queries.
	lastPhase map[string]string // key: jobID, value: "phase|detail"

	// knownJobsMu guards knownJobs independently of progressMu — job
	// lifecycle detection and progress storage are separate concerns.
	knownJobsMu sync.Mutex
	knownJobs   map[string]bool // key: jobID → true if seen
}

// NewAggregator creates a telemetry aggregator.
func NewAggregator(upstream *vigilclient.TelemetryClient, registry *AgentRegistry, psk string, thresholds AlertThresholds, logger *slog.Logger) *Aggregator {
	return &Aggregator{
		upstream:   upstream,
		registry:   registry,
		psk:        psk,
		thresholds: thresholds,
		logger:     logger,
		conns:          make(map[string]*agentConn),
		lastPhase:      make(map[string]string),
		smartDeltas:    make(map[string]json.RawMessage),
		activeProgress: make(map[string]ProgressPayload),
		knownJobs:      make(map[string]bool),
	}
}

// SetUpstream replaces the upstream telemetry client. This is called once
// the Vigil registration completes and the TelemetryClient is available.
func (a *Aggregator) SetUpstream(t *vigilclient.TelemetryClient) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.upstream = t
}

// HandleAgentTelemetry is the HTTP handler for GET /api/agents/{id}/telemetry.
// It upgrades the connection to WebSocket and reads telemetry frames from the agent.
func (a *Aggregator) HandleAgentTelemetry(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "agent id required", http.StatusBadRequest)
		return
	}

	// Validate PSK.
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != a.psk {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Verify agent is registered.
	if agent := a.registry.Get(agentID); agent == nil {
		http.Error(w, "agent not registered", http.StatusNotFound)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		a.logger.Error("websocket upgrade failed", "agent_id", agentID, "error", err)
		return
	}

	// Mark agent as seen on connect.
	a.registry.TouchLastSeen(agentID)

	ac := &agentConn{agentID: agentID, conn: conn}

	a.mu.Lock()
	// Close any existing connection from the same agent.
	if old, ok := a.conns[agentID]; ok {
		old.conn.Close()
		a.logger.Info("replaced existing telemetry connection", "agent_id", agentID)
	}
	a.conns[agentID] = ac
	connCount := len(a.conns)
	a.mu.Unlock()

	a.logger.Info("agent telemetry connected", "agent_id", agentID, "active_connections", connCount)

	defer func() {
		conn.Close()
		a.mu.Lock()
		// Only remove if it's still our connection (not replaced by a newer one).
		if cur, ok := a.conns[agentID]; ok && cur == ac {
			delete(a.conns, agentID)
		}
		connCount := len(a.conns)
		a.mu.Unlock()
		a.logger.Info("agent telemetry disconnected", "agent_id", agentID, "active_connections", connCount)
	}()

	a.readLoop(ac)
}

// agentFrame is the flat telemetry frame structure sent by agents.
type agentFrame struct {
	Type         string          `json:"type"`
	AgentID      string          `json:"agent_id"`
	JobID        string          `json:"job_id"`
	ComponentID  string          `json:"component_id,omitempty"`
	Command      string          `json:"command"`
	Phase        string          `json:"phase"`
	PhaseDetail  string          `json:"phase_detail,omitempty"`
	Percent      float64         `json:"percent"`
	SpeedMbps    float64         `json:"speed_mbps,omitempty"`
	TempC        int             `json:"temp_c,omitempty"`
	ElapsedSec   int64           `json:"elapsed_sec,omitempty"`
	ETASec       int64           `json:"eta_sec,omitempty"`
	BadblockErrs int             `json:"badblocks_errors,omitempty"`
	SmartDeltas  json.RawMessage `json:"smart_deltas,omitempty"`
	Level        string          `json:"level,omitempty"`    // preferred — matches Vigil UI
	Severity     string          `json:"severity,omitempty"` // legacy fallback
	Message      string          `json:"message,omitempty"`
	Source       string          `json:"source,omitempty"`
	Timestamp    string          `json:"timestamp,omitempty"`
	Key          string          `json:"key,omitempty"`   // metric/chart frames
	Value        float64         `json:"value,omitempty"` // metric/chart frames
}

// resolveLevel returns the log level from an agent frame, preferring "level"
// over the legacy "severity" field for backward compatibility.
func (f *agentFrame) resolveLevel() string {
	if f.Level != "" {
		return f.Level
	}
	return f.Severity
}

const agentPingInterval = 30 * time.Second

func (a *Aggregator) readLoop(ac *agentConn) {
	ac.conn.SetReadLimit(maxMessageSize)
	ac.conn.SetReadDeadline(time.Now().Add(90 * time.Second))

	// Extend read deadline and touch registry on pong from agent.
	ac.conn.SetPongHandler(func(string) error {
		ac.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		a.registry.TouchLastSeen(ac.agentID)
		return nil
	})

	// Ping loop — keeps the agent connection alive and drives LastSeenAt.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(agentPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := ac.conn.WriteControl(
					websocket.PingMessage, nil,
					time.Now().Add(10*time.Second),
				); err != nil {
					return
				}
			}
		}
	}()
	defer close(done)

	for {
		_, msg, err := ac.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				a.logger.Warn("agent telemetry read error", "agent_id", ac.agentID, "error", err)
			}
			return
		}

		// Reset read deadline on any data frame.
		ac.conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		var frame agentFrame
		if err := json.Unmarshal(msg, &frame); err != nil {
			a.logger.Warn("invalid telemetry frame from agent", "agent_id", ac.agentID, "error", err)
			continue
		}

		// Enforce agent_id tagging — always use the authenticated connection's ID.
		frame.AgentID = ac.agentID

		a.logger.Debug("agent frame received",
			"agent_id", ac.agentID,
			"type", frame.Type,
			"job_id", frame.JobID,
		)

		// Keep the agent's last-seen timestamp fresh.
		a.registry.TouchLastSeen(ac.agentID)

		a.processFrame(frame)
	}
}

func (a *Aggregator) processFrame(frame agentFrame) {
	a.mu.Lock()
	upstream := a.upstream
	a.mu.Unlock()

	// Always store data locally so dashboard queries work even when
	// the upstream Vigil connection is not yet established.
	switch frame.Type {
	case "progress":
		a.logger.Info("processing progress frame",
			"agent_id", frame.AgentID,
			"job_id", frame.JobID,
			"phase", frame.Phase,
			"percent", frame.Percent,
		)
		a.storeProgressLocally(frame)
		a.storePhaseTransition(frame)
		a.cleanupIfComplete(frame)
		if upstream != nil {
			a.forwardProgress(upstream, frame)
			a.evaluateProgress(upstream, frame)
		}
	case "log":
		a.logger.Info("processing log frame",
			"agent_id", frame.AgentID,
			"job_id", frame.JobID,
			"level", frame.resolveLevel(),
		)
		a.storeLogLocally(frame)
		if upstream != nil {
			a.forwardLog(upstream, frame)
			a.evaluateLog(upstream, frame)
		}
	case "metric":
		a.logger.Debug("processing metric frame",
			"agent_id", frame.AgentID,
			"key", frame.Key,
		)
		if upstream != nil {
			a.forwardMetric(upstream, frame)
		}
	case "chart":
		a.logger.Debug("processing chart frame",
			"agent_id", frame.AgentID,
			"component_id", frame.ComponentID,
			"key", frame.Key,
		)
		a.storeChartLocally(frame)
		if upstream != nil {
			a.forwardChart(upstream, frame)
		}
	default:
		a.logger.Warn("unknown frame type from agent", "agent_id", frame.AgentID, "type", frame.Type)
	}
}

// storeProgressLocally persists the latest progress and SMART deltas for a
// job so that dashboard queries work independently of the upstream connection.
func (a *Aggregator) storeProgressLocally(frame agentFrame) {
	if frame.JobID == "" {
		return
	}
	p := ProgressPayload{
		AgentID:      frame.AgentID,
		JobID:        frame.JobID,
		Command:      frame.Command,
		Phase:        frame.Phase,
		PhaseDetail:  frame.PhaseDetail,
		Percent:      frame.Percent,
		SpeedMbps:    frame.SpeedMbps,
		TempC:        frame.TempC,
		ElapsedSec:   frame.ElapsedSec,
		ETASec:       frame.ETASec,
		BadblockErrs: frame.BadblockErrs,
		SmartDeltas:  frame.SmartDeltas,
	}
	a.storeProgress(frame.JobID, p)
	if len(frame.SmartDeltas) > 0 {
		a.storeSmartDeltas(frame.JobID, frame.SmartDeltas)
	}
}

// storeLogLocally persists a log entry to the ring buffer independently of upstream.
func (a *Aggregator) storeLogLocally(frame agentFrame) {
	ts := frame.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	source := frame.Source
	if source == "" {
		source = frame.AgentID
	}
	a.storeLog(StoredLog{
		Level:     frame.resolveLevel(),
		Message:   frame.Message,
		Source:    source,
		JobID:     frame.JobID,
		Timestamp: ts,
	})
}

// storeChartLocally persists a chart data point independently of upstream.
func (a *Aggregator) storeChartLocally(frame agentFrame) {
	ts := frame.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	a.storeChartPoint(StoredChartPoint{
		ComponentID: frame.ComponentID,
		Key:         frame.Key,
		Value:       frame.Value,
		Source:      frame.AgentID,
		Timestamp:   ts,
	})
}

func (a *Aggregator) forwardProgress(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	p := ProgressPayload{
		AgentID:      frame.AgentID,
		JobID:        frame.JobID,
		Command:      frame.Command,
		Phase:        frame.Phase,
		PhaseDetail:  frame.PhaseDetail,
		Percent:      frame.Percent,
		SpeedMbps:    frame.SpeedMbps,
		TempC:        frame.TempC,
		ElapsedSec:   frame.ElapsedSec,
		ETASec:       frame.ETASec,
		BadblockErrs: frame.BadblockErrs,
		SmartDeltas:  frame.SmartDeltas,
	}

	if err := upstream.Send("progress", p); err != nil {
		a.logger.Warn("failed to relay progress upstream",
			"agent_id", frame.AgentID,
			"job_id", frame.JobID,
			"phase", frame.Phase,
			"error", err,
		)
	}
}

func (a *Aggregator) forwardMetric(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	ts := frame.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	m := MetricPayload{
		Key:       frame.Key,
		Value:     frame.Value,
		Timestamp: ts,
	}
	if err := upstream.Send("metric", m); err != nil {
		a.logger.Warn("failed to relay metric upstream",
			"agent_id", frame.AgentID,
			"key", frame.Key,
			"error", err,
		)
	}
}

func (a *Aggregator) forwardChart(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	ts := frame.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	c := ChartPayload{
		ComponentID: frame.ComponentID,
		Key:         frame.Key,
		Value:       frame.Value,
		Timestamp:   ts,
	}

	if err := upstream.Send("chart", c); err != nil {
		a.logger.Warn("failed to relay chart upstream",
			"agent_id", frame.AgentID,
			"component_id", frame.ComponentID,
			"key", frame.Key,
			"error", err,
		)
	}
}

func (a *Aggregator) forwardLog(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	ts := frame.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	source := frame.Source
	if source == "" {
		source = frame.AgentID
	}
	l := vigilclient.LogPayload{
		ComponentID: frame.ComponentID,
		Level:       frame.resolveLevel(),
		Message:     frame.Message,
		Source:      source,
		JobID:       frame.JobID,
		Timestamp:   ts,
	}

	if err := upstream.Send("log", l); err != nil {
		a.logger.Warn("failed to relay log upstream",
			"agent_id", frame.AgentID,
			"job_id", frame.JobID,
			"error", err,
		)
	}
}

// storeLog appends a log entry to the ring buffer, evicting the oldest
// entry when capacity is exceeded.
func (a *Aggregator) storeLog(entry StoredLog) {
	a.logMu.Lock()
	defer a.logMu.Unlock()

	a.logRing = append(a.logRing, entry)
	if len(a.logRing) > maxStoredLogs {
		// Drop the oldest 10% to avoid constant slice shifting.
		drop := maxStoredLogs / 10
		copy(a.logRing, a.logRing[drop:])
		a.logRing = a.logRing[:len(a.logRing)-drop]
	}
}

// storePhaseTransition checks whether a progress frame represents a phase
// change and, if so, stores a synthetic log entry in the ring buffer. This
// ensures that historical log queries (used by the Dashboard "Recent Activity"
// and History "Job Logs" viewers) include a complete timeline of phase
// transitions, not just explicit log frames from the agent.
func (a *Aggregator) storePhaseTransition(frame agentFrame) {
	if frame.Phase == "" || frame.JobID == "" {
		return
	}

	detail := frame.PhaseDetail
	phaseKey := frame.Phase + "|" + detail

	a.logMu.Lock()
	prev := a.lastPhase[frame.JobID]
	if prev == phaseKey {
		a.logMu.Unlock()
		return
	}
	a.lastPhase[frame.JobID] = phaseKey
	a.logMu.Unlock()

	// Build a human-readable message matching the client-side format.
	msg := frame.Phase
	if detail != "" {
		msg += " — " + detail
	}

	source := frame.AgentID
	if source == "" {
		source = "hub"
	}

	a.storeLog(StoredLog{
		Level:     "info",
		Message:   msg,
		Source:    source,
		JobID:     frame.JobID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// CleanupJobPhase removes the tracked phase for a completed job to prevent
// unbounded growth of the lastPhase map.
func (a *Aggregator) CleanupJobPhase(jobID string) {
	a.logMu.Lock()
	delete(a.lastPhase, jobID)
	a.logMu.Unlock()
}

// cleanupIfComplete removes tracking state for completed or cancelled jobs
// so it runs regardless of upstream connectivity.
func (a *Aggregator) cleanupIfComplete(frame agentFrame) {
	if frame.Percent >= 100.0 && (frame.Phase == "COMPLETE" || frame.Phase == "CANCELLED") {
		a.CleanupJobPhase(frame.JobID)
		a.cleanupSmartDeltas(frame.JobID)
		a.cleanupProgress(frame.JobID)
	}
}

// storeSmartDeltas saves the latest enriched SMART deltas for a job.
func (a *Aggregator) storeSmartDeltas(jobID string, deltas json.RawMessage) {
	a.smartMu.Lock()
	defer a.smartMu.Unlock()
	a.smartDeltas[jobID] = deltas
}

// cleanupSmartDeltas removes stored SMART deltas for a completed job.
func (a *Aggregator) cleanupSmartDeltas(jobID string) {
	a.smartMu.Lock()
	defer a.smartMu.Unlock()
	delete(a.smartDeltas, jobID)
}

// QuerySmartDeltas returns the latest stored SMART deltas across all active
// jobs, merged into a single enriched map. If only one job is running this
// is equivalent to that job's latest deltas.
func (a *Aggregator) QuerySmartDeltas() json.RawMessage {
	a.smartMu.Lock()
	defer a.smartMu.Unlock()

	if len(a.smartDeltas) == 0 {
		return nil
	}

	// If there's exactly one job, return its deltas directly.
	if len(a.smartDeltas) == 1 {
		for _, v := range a.smartDeltas {
			return v
		}
	}

	// Multiple jobs: merge all deltas into a single map keyed by job ID.
	merged := make(map[string]json.RawMessage, len(a.smartDeltas))
	for jobID, v := range a.smartDeltas {
		merged[jobID] = v
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return nil
	}
	return data
}

// storeProgress saves the latest progress payload for an active job.
func (a *Aggregator) storeProgress(jobID string, p ProgressPayload) {
	a.progressMu.Lock()
	defer a.progressMu.Unlock()
	a.activeProgress[jobID] = p
}

// cleanupProgress removes stored progress for a completed job.
func (a *Aggregator) cleanupProgress(jobID string) {
	a.progressMu.Lock()
	defer a.progressMu.Unlock()
	delete(a.activeProgress, jobID)
}

// QueryActiveJobs returns the latest progress state for all active jobs.
func (a *Aggregator) QueryActiveJobs() []ProgressPayload {
	a.progressMu.Lock()
	defer a.progressMu.Unlock()

	jobs := make([]ProgressPayload, 0, len(a.activeProgress))
	for _, p := range a.activeProgress {
		jobs = append(jobs, p)
	}
	return jobs
}

// BusyAgentIDs returns a set of agent IDs that currently have active jobs.
func (a *Aggregator) BusyAgentIDs() map[string]bool {
	a.progressMu.Lock()
	defer a.progressMu.Unlock()

	busy := make(map[string]bool, len(a.activeProgress))
	for _, p := range a.activeProgress {
		busy[p.AgentID] = true
	}
	return busy
}

// QueryLogs returns stored log entries, optionally filtered to those
// with a timestamp within the given time range. An empty timeRange
// returns all stored logs.
func (a *Aggregator) QueryLogs(timeRange string) []StoredLog {
	a.logMu.Lock()
	snapshot := make([]StoredLog, len(a.logRing))
	copy(snapshot, a.logRing)
	a.logMu.Unlock()

	dur, ok := addonutil.ParseTimeRange(timeRange)
	if !ok || timeRange == "" {
		return snapshot
	}

	cutoff := time.Now().Add(-dur)
	filtered := make([]StoredLog, 0, len(snapshot))
	for _, entry := range snapshot {
		t, err := time.Parse(time.RFC3339, entry.Timestamp)
		if err != nil {
			filtered = append(filtered, entry) // Fail-open.
			continue
		}
		if t.After(cutoff) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// storeChartPoint appends a chart data point to the chart ring buffer.
func (a *Aggregator) storeChartPoint(point StoredChartPoint) {
	a.chartMu.Lock()
	defer a.chartMu.Unlock()

	a.chartRing = append(a.chartRing, point)
	if len(a.chartRing) > maxStoredChartPoints {
		drop := maxStoredChartPoints / 10
		copy(a.chartRing, a.chartRing[drop:])
		a.chartRing = a.chartRing[:len(a.chartRing)-drop]
	}
}

// QueryChartHistory returns stored chart data points for a given component,
// optionally filtered by time range.
func (a *Aggregator) QueryChartHistory(componentID, timeRange string) []StoredChartPoint {
	a.chartMu.Lock()
	snapshot := make([]StoredChartPoint, len(a.chartRing))
	copy(snapshot, a.chartRing)
	a.chartMu.Unlock()

	dur, ok := addonutil.ParseTimeRange(timeRange)

	filtered := make([]StoredChartPoint, 0, len(snapshot))
	for _, pt := range snapshot {
		if componentID != "" && pt.ComponentID != componentID {
			continue
		}
		if ok && timeRange != "" {
			t, err := time.Parse(time.RFC3339, pt.Timestamp)
			if err == nil && !t.After(time.Now().Add(-dur)) {
				continue
			}
		}
		filtered = append(filtered, pt)
	}
	return filtered
}

// evaluateProgress inspects progress frames for notification triggers.
func (a *Aggregator) evaluateProgress(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	if frame.JobID != "" {
		a.evaluateJobLifecycle(upstream, frame)
	}

	// Temperature alerts.
	if frame.TempC > 0 {
		a.checkTemperature(upstream, frame)
	}

	// SMART delta warnings (any critical attribute delta > 0).
	if len(frame.SmartDeltas) > 0 {
		a.checkSmartDeltas(upstream, frame)
	}

	// Badblock error detection.
	if frame.BadblockErrs > 0 {
		a.emitNotification(upstream, frame, "job_failed", "critical",
			fmt.Sprintf("🔴 Bad blocks detected on drive (%s): %d errors found", frame.AgentID, frame.BadblockErrs))
	}

	// Phase completion (100% signals a phase is done).
	if frame.Percent >= 100.0 && frame.Phase != "" {
		a.emitNotification(upstream, frame, "phase_complete", "info",
			fmt.Sprintf("🔄 Phase %q completed on agent %s", frame.Phase, frame.AgentID))

		// Special case: burn-in complete in a full pipeline.
		if frame.Command == "full" && frame.Phase == "complete" {
			a.emitNotification(upstream, frame, "burnin_passed", "info",
				fmt.Sprintf("✅ Burn-in passed on agent %s, pre-clear beginning", frame.AgentID))
		}

	}
}

// evaluateJobLifecycle detects job_started and job_complete transitions.
func (a *Aggregator) evaluateJobLifecycle(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	a.knownJobsMu.Lock()
	seen := a.knownJobs[frame.JobID]
	a.knownJobsMu.Unlock()

	if !seen {
		// First progress frame for this job — mark as started.
		a.knownJobsMu.Lock()
		a.knownJobs[frame.JobID] = true
		a.knownJobsMu.Unlock()

		cmd := frame.Command
		if cmd == "" {
			cmd = "job"
		}
		a.emitNotification(upstream, frame, "job_started", "info",
			fmt.Sprintf("▶️ Burn-in %s started on agent %s (job %s)", cmd, frame.AgentID, frame.JobID))
	}

	// Detect job completion.
	if frame.Percent >= 100.0 && (frame.Phase == "COMPLETE" || frame.Phase == "CANCELLED") {
		a.knownJobsMu.Lock()
		delete(a.knownJobs, frame.JobID)
		a.knownJobsMu.Unlock()

		status := "completed"
		if frame.Phase == "CANCELLED" {
			status = "cancelled"
		}
		cmd := frame.Command
		if cmd == "" {
			cmd = "job"
		}
		icon := "✅"
		if status == "cancelled" {
			icon = "🛑"
		}
		a.emitNotification(upstream, frame, "job_complete", "info",
			fmt.Sprintf("%s Burn-in %s %s on agent %s (job %s)", icon, cmd, status, frame.AgentID, frame.JobID))
	}
}

// evaluateLog inspects log frames for notification triggers.
func (a *Aggregator) evaluateLog(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	switch frame.resolveLevel() {
	case "error":
		a.emitNotification(upstream, frame, "job_failed", "critical",
			fmt.Sprintf("🔴 Agent %s reported error: %s", frame.AgentID, frame.Message))
	case "warning", "warn":
		a.emitNotification(upstream, frame, "smart_warning", "warning",
			fmt.Sprintf("⚠️ Agent %s warning: %s", frame.AgentID, frame.Message))
	}
}

func (a *Aggregator) checkTemperature(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	if frame.TempC >= a.thresholds.TempCriticalC {
		a.emitNotification(upstream, frame, "temp_alert", "critical",
			fmt.Sprintf("🔴 Drive temperature critical on agent %s: %d°C (threshold: %d°C)",
				frame.AgentID, frame.TempC, a.thresholds.TempCriticalC))
	} else if frame.TempC >= a.thresholds.TempWarningC {
		a.emitNotification(upstream, frame, "temp_alert", "warning",
			fmt.Sprintf("⚠️ Drive temperature warning on agent %s: %d°C (threshold: %d°C)",
				frame.AgentID, frame.TempC, a.thresholds.TempWarningC))
	}
}

// smartDelta is used to parse individual SMART attribute entries.
type smartDelta struct {
	Name     string `json:"name"`
	Baseline int    `json:"baseline"`
	Current  int    `json:"current"`
}

func (a *Aggregator) checkSmartDeltas(upstream *vigilclient.TelemetryClient, frame agentFrame) {
	var deltas map[string]smartDelta
	if err := json.Unmarshal(frame.SmartDeltas, &deltas); err != nil {
		return
	}

	for id, d := range deltas {
		delta := d.Current - d.Baseline
		if delta > 0 {
			a.emitNotification(upstream, frame, "smart_warning", "warning",
				fmt.Sprintf("⚠️ SMART attribute %s (%s) +%d on agent %s (baseline: %d → current: %d)",
					id, d.Name, delta, frame.AgentID, d.Baseline, d.Current))
		}
	}
}

func (a *Aggregator) emitNotification(upstream *vigilclient.TelemetryClient, frame agentFrame, eventType, severity, message string) {
	source := fmt.Sprintf("addon:burnin-preclear-v1:agent:%s", frame.AgentID)
	if frame.JobID != "" {
		source += ":job:" + frame.JobID
	}

	n := vigilclient.NotificationPayload{
		EventType: eventType,
		Severity:  severity,
		Source:    source,
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	if err := upstream.Send("notification", n); err != nil {
		a.logger.Warn("failed to transmit notification upstream",
			"event_type", eventType,
			"agent_id", frame.AgentID,
			"error", err,
		)
	}

	a.logger.Info("notification emitted",
		"event_type", eventType,
		"severity", severity,
		"agent_id", frame.AgentID,
	)
}
