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
)

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

// Aggregator accepts telemetry WebSocket connections from agents,
// evaluates frames for notification triggers, and multiplexes
// everything into the single upstream TelemetryClient.
type Aggregator struct {
	upstream   *TelemetryClient
	registry   *AgentRegistry
	psk        string
	thresholds AlertThresholds
	logger     *slog.Logger

	mu    sync.Mutex
	conns map[string]*agentConn
}

// NewAggregator creates a telemetry aggregator.
func NewAggregator(upstream *TelemetryClient, registry *AgentRegistry, psk string, thresholds AlertThresholds, logger *slog.Logger) *Aggregator {
	return &Aggregator{
		upstream:   upstream,
		registry:   registry,
		psk:        psk,
		thresholds: thresholds,
		logger:     logger,
		conns:      make(map[string]*agentConn),
	}
}

// SetUpstream replaces the upstream telemetry client. This is called once
// the Vigil registration completes and the TelemetryClient is available.
func (a *Aggregator) SetUpstream(t *TelemetryClient) {
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

	a.logger.Info("agent telemetry connected", "agent_id", agentID)

	ac := &agentConn{agentID: agentID, conn: conn}

	a.mu.Lock()
	// Close any existing connection from the same agent.
	if old, ok := a.conns[agentID]; ok {
		old.conn.Close()
	}
	a.conns[agentID] = ac
	a.mu.Unlock()

	defer func() {
		conn.Close()
		a.mu.Lock()
		// Only remove if it's still our connection (not replaced by a newer one).
		if cur, ok := a.conns[agentID]; ok && cur == ac {
			delete(a.conns, agentID)
		}
		a.mu.Unlock()
		a.logger.Info("agent telemetry disconnected", "agent_id", agentID)
	}()

	a.readLoop(ac)
}

// agentFrame is the flat telemetry frame structure sent by agents.
type agentFrame struct {
	Type         string          `json:"type"`
	AgentID      string          `json:"agent_id"`
	JobID        string          `json:"job_id"`
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
	Severity     string          `json:"severity,omitempty"`
	Message      string          `json:"message,omitempty"`
	Timestamp    string          `json:"timestamp,omitempty"`
}

func (a *Aggregator) readLoop(ac *agentConn) {
	ac.conn.SetReadLimit(maxMessageSize)

	for {
		_, msg, err := ac.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				a.logger.Warn("agent telemetry read error", "agent_id", ac.agentID, "error", err)
			}
			return
		}

		var frame agentFrame
		if err := json.Unmarshal(msg, &frame); err != nil {
			a.logger.Warn("invalid telemetry frame from agent", "agent_id", ac.agentID, "error", err)
			continue
		}

		// Enforce agent_id tagging — always use the authenticated connection's ID.
		frame.AgentID = ac.agentID

		a.processFrame(frame)
	}
}

func (a *Aggregator) processFrame(frame agentFrame) {
	a.mu.Lock()
	upstream := a.upstream
	a.mu.Unlock()

	if upstream == nil {
		return
	}

	switch frame.Type {
	case "progress":
		a.forwardProgress(upstream, frame)
		a.evaluateProgress(upstream, frame)
	case "log":
		a.forwardLog(upstream, frame)
		a.evaluateLog(upstream, frame)
	default:
		a.logger.Warn("unknown frame type from agent", "agent_id", frame.AgentID, "type", frame.Type)
	}
}

func (a *Aggregator) forwardProgress(upstream *TelemetryClient, frame agentFrame) {
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
	if err := upstream.SendProgress(p); err != nil {
		a.logger.Warn("failed to relay progress upstream", "agent_id", frame.AgentID, "error", err)
	}
}

func (a *Aggregator) forwardLog(upstream *TelemetryClient, frame agentFrame) {
	ts := frame.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	l := LogPayload{
		AgentID:   frame.AgentID,
		JobID:     frame.JobID,
		Severity:  frame.Severity,
		Message:   frame.Message,
		Timestamp: ts,
	}
	if err := upstream.SendLog(l); err != nil {
		a.logger.Warn("failed to relay log upstream", "agent_id", frame.AgentID, "error", err)
	}
}

// evaluateProgress inspects progress frames for notification triggers.
func (a *Aggregator) evaluateProgress(upstream *TelemetryClient, frame agentFrame) {
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
		a.emitNotification(upstream, frame, "JobFailed", "critical",
			fmt.Sprintf("Bad blocks detected on drive (%s): %d errors found", frame.AgentID, frame.BadblockErrs))
	}

	// Phase completion (100% signals a phase is done).
	if frame.Percent >= 100.0 && frame.Phase != "" {
		a.emitNotification(upstream, frame, "PhaseComplete", "info",
			fmt.Sprintf("Phase %q completed on agent %s", frame.Phase, frame.AgentID))

		// Special case: burn-in complete in a full pipeline.
		if frame.Command == "full" && frame.Phase == "complete" {
			a.emitNotification(upstream, frame, "BurninPassed", "info",
				fmt.Sprintf("Burn-in passed on agent %s, pre-clear beginning", frame.AgentID))
		}
	}
}

// evaluateLog inspects log frames for notification triggers.
func (a *Aggregator) evaluateLog(upstream *TelemetryClient, frame agentFrame) {
	switch frame.Severity {
	case "error":
		a.emitNotification(upstream, frame, "JobFailed", "critical",
			fmt.Sprintf("Agent %s reported error: %s", frame.AgentID, frame.Message))
	}
}

func (a *Aggregator) checkTemperature(upstream *TelemetryClient, frame agentFrame) {
	if frame.TempC >= a.thresholds.TempCriticalC {
		a.emitNotification(upstream, frame, "TempAlert", "critical",
			fmt.Sprintf("Drive temperature critical on agent %s: %d°C (threshold: %d°C)",
				frame.AgentID, frame.TempC, a.thresholds.TempCriticalC))
	} else if frame.TempC >= a.thresholds.TempWarningC {
		a.emitNotification(upstream, frame, "TempAlert", "warning",
			fmt.Sprintf("Drive temperature warning on agent %s: %d°C (threshold: %d°C)",
				frame.AgentID, frame.TempC, a.thresholds.TempWarningC))
	}
}

// smartDelta is used to parse individual SMART attribute entries.
type smartDelta struct {
	Name     string `json:"name"`
	Baseline int    `json:"baseline"`
	Current  int    `json:"current"`
}

func (a *Aggregator) checkSmartDeltas(upstream *TelemetryClient, frame agentFrame) {
	var deltas map[string]smartDelta
	if err := json.Unmarshal(frame.SmartDeltas, &deltas); err != nil {
		return
	}

	for id, d := range deltas {
		delta := d.Current - d.Baseline
		if delta > 0 {
			a.emitNotification(upstream, frame, "SmartWarning", "warning",
				fmt.Sprintf("SMART attribute %s (%s) increased by %d on agent %s (baseline: %d, current: %d)",
					id, d.Name, delta, frame.AgentID, d.Baseline, d.Current))
		}
	}
}

func (a *Aggregator) emitNotification(upstream *TelemetryClient, frame agentFrame, eventType, severity, message string) {
	source := fmt.Sprintf("addon:burnin-preclear-v1:agent:%s", frame.AgentID)
	if frame.JobID != "" {
		source += ":job:" + frame.JobID
	}

	n := NotificationPayload{
		EventType: eventType,
		Severity:  severity,
		Source:    source,
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	if err := upstream.SendNotification(n); err != nil {
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
