package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	telemetryPath  = "/api/agents/%s/telemetry"
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	maxMessageSize = 64 * 1024 // 64 KB
)

// ProgressFrame is the flat progress telemetry frame sent to the hub.
type ProgressFrame struct {
	Type         string          `json:"type"`
	AgentID      string          `json:"agent_id"`
	JobID        string          `json:"job_id"`
	Command      string          `json:"command"`
	Phase        string          `json:"phase"`
	PhaseDetail  string          `json:"phase_detail,omitempty"`
	Percent      float64         `json:"percent"`
	SpeedMbps    float64         `json:"speed_mbps,omitempty"`
	TempC        int             `json:"temp_c,omitempty"`
	ElapsedSec   int64           `json:"elapsed_sec"`
	ETASec       int64           `json:"eta_sec,omitempty"`
	BadblockErrs int             `json:"badblocks_errors,omitempty"`
	SmartDeltas  json.RawMessage `json:"smart_deltas,omitempty"`
}

// LogFrame is the flat log telemetry frame sent to the hub.
type LogFrame struct {
	Type      string `json:"type"`
	AgentID   string `json:"agent_id"`
	JobID     string `json:"job_id"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// HubTelemetry manages the persistent WebSocket connection to the hub
// for streaming telemetry frames.
type HubTelemetry struct {
	hubURL  string
	agentID string
	psk     string
	logger  *slog.Logger

	mu   sync.Mutex
	conn *websocket.Conn
}

// NewHubTelemetry creates a telemetry client for the hub connection.
func NewHubTelemetry(hubURL, agentID, psk string, logger *slog.Logger) *HubTelemetry {
	return &HubTelemetry{
		hubURL:  hubURL,
		agentID: agentID,
		psk:     psk,
		logger:  logger,
	}
}

// Run connects to the hub and maintains the WebSocket indefinitely.
// It blocks until the context is cancelled.
func (t *HubTelemetry) Run(ctx context.Context) {
	var attempt int
	for {
		err := t.connectAndServe(ctx)
		if ctx.Err() != nil {
			t.logger.Info("hub telemetry client stopped")
			return
		}

		attempt++
		delay := backoffDelay(attempt)
		t.logger.Warn("hub websocket disconnected, reconnecting",
			"error", err,
			"attempt", attempt,
			"retry_in", delay,
		)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (t *HubTelemetry) connectAndServe(ctx context.Context) error {
	wsURL := t.wsURL()
	t.logger.Info("connecting to hub telemetry", "url", wsURL)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+t.psk)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer func() {
		conn.Close()
		t.mu.Lock()
		t.conn = nil
		t.mu.Unlock()
	}()

	conn.SetReadLimit(maxMessageSize)
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	t.mu.Lock()
	t.conn = conn
	t.mu.Unlock()

	t.logger.Info("hub telemetry websocket connected")

	// Reset backoff on successful connection.
	// Read loop — keeps the connection alive and detects closure.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return fmt.Errorf("read: %w", err)
		}
	}
}

// SendProgress transmits a progress frame to the hub.
func (t *HubTelemetry) SendProgress(jobID, command, phase, phaseDetail string, percent, speedMbps float64, tempC int, elapsedSec, etaSec int64, badblockErrs int, smartDeltas json.RawMessage) error {
	frame := ProgressFrame{
		Type:         "progress",
		AgentID:      t.agentID,
		JobID:        jobID,
		Command:      command,
		Phase:        phase,
		PhaseDetail:  phaseDetail,
		Percent:      percent,
		SpeedMbps:    speedMbps,
		TempC:        tempC,
		ElapsedSec:   elapsedSec,
		ETASec:       etaSec,
		BadblockErrs: badblockErrs,
		SmartDeltas:  smartDeltas,
	}
	return t.writeJSON(frame)
}

// SendLog transmits a log frame to the hub.
func (t *HubTelemetry) SendLog(jobID, severity, message string) error {
	frame := LogFrame{
		Type:      "log",
		AgentID:   t.agentID,
		JobID:     jobID,
		Severity:  severity,
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	return t.writeJSON(frame)
}

func (t *HubTelemetry) writeJSON(v any) error {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("hub websocket not connected")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}
	return conn.WriteJSON(v)
}

func (t *HubTelemetry) wsURL() string {
	base := t.hubURL
	base = strings.Replace(base, "https://", "wss://", 1)
	base = strings.Replace(base, "http://", "ws://", 1)
	return fmt.Sprintf("%s"+telemetryPath, base, t.agentID)
}
