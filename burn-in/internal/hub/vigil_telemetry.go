package hub

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
	wsPath         = "/api/addons/ws"
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	maxMessageSize = 64 * 1024 // 64 KB per Vigil spec
)

// TelemetryFrame is the envelope for all frames sent upstream.
type TelemetryFrame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ProgressPayload is the payload for a progress telemetry frame.
type ProgressPayload struct {
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

// LogPayload is the payload for a log telemetry frame.
// Field names match the Vigil UI contract: "level" and "source".
type LogPayload struct {
	Level     string `json:"level"`
	Message   string `json:"message"`
	Source    string `json:"source,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// NotificationPayload is the payload for a notification telemetry frame.
type NotificationPayload struct {
	EventType string `json:"event_type"`
	Severity  string `json:"severity"`
	Source    string `json:"source"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// TelemetryClient manages the persistent WebSocket connection to the Vigil server.
type TelemetryClient struct {
	serverURL     string
	addonID       int64
	agentToken    string
	heartbeatInt  time.Duration
	logger        *slog.Logger

	mu    sync.Mutex
	conn  *websocket.Conn
	ready chan struct{} // closed once the first connection succeeds
	once  sync.Once
}

// NewTelemetryClient creates an upstream telemetry client.
func NewTelemetryClient(serverURL string, addonID int64, agentToken string, heartbeatInterval time.Duration, logger *slog.Logger) *TelemetryClient {
	return &TelemetryClient{
		serverURL:    serverURL,
		addonID:      addonID,
		agentToken:   agentToken,
		heartbeatInt: heartbeatInterval,
		logger:       logger,
		ready:        make(chan struct{}),
	}
}

// Ready returns a channel that is closed when the first upstream connection
// is established. Callers can select on this to delay operations until the
// telemetry pipeline is active.
func (t *TelemetryClient) Ready() <-chan struct{} {
	return t.ready
}

// Run connects to the Vigil server and maintains the connection indefinitely.
// It blocks until the context is cancelled.
func (t *TelemetryClient) Run(ctx context.Context) {
	var attempt int
	for {
		err := t.connectAndServe(ctx)
		if ctx.Err() != nil {
			t.logger.Info("telemetry client stopped")
			return
		}

		attempt++
		delay := backoffDelay(attempt)
		t.logger.Warn("upstream websocket disconnected, reconnecting",
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

func (t *TelemetryClient) connectAndServe(ctx context.Context) error {
	wsURL := t.wsURL()
	t.logger.Info("connecting to vigil upstream", "url", wsURL)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+t.agentToken)

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

	// Set initial read deadline — the server sends pings every 30s,
	// so we allow up to pongWait (60s) before considering it dead.
	conn.SetReadDeadline(time.Now().Add(pongWait))

	// Handle pings FROM the Vigil server: extend the read deadline and
	// reply with a pong (the default handler only sends pong but doesn't
	// touch the deadline).
	conn.SetPingHandler(func(appData string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return conn.WriteControl(websocket.PongMessage,
			[]byte(appData), time.Now().Add(writeWait))
	})

	t.mu.Lock()
	t.conn = conn
	t.mu.Unlock()

	// Signal that the upstream pipeline is ready for the first time.
	t.once.Do(func() { close(t.ready) })

	t.logger.Info("upstream websocket connected",
		"addon_id", t.addonID,
		"url", wsURL,
	)

	// Heartbeat loop in a separate goroutine.
	// If the heartbeat write fails, close the connection to unblock ReadMessage.
	heartCtx, heartCancel := context.WithCancel(ctx)
	defer heartCancel()

	go func() {
		t.heartbeatLoop(heartCtx, conn)
		// Heartbeat loop exited (write failure or context cancelled).
		// Close the connection so the read loop below unblocks.
		conn.Close()
	}()

	// Read loop — keeps the connection alive and detects closure.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return fmt.Errorf("read: %w", err)
		}
	}
}

func (t *TelemetryClient) heartbeatLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(t.heartbeatInt)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frame := TelemetryFrame{Type: "heartbeat"}
			if err := t.writeFrame(conn, frame); err != nil {
				t.logger.Warn("failed to transmit heartbeat", "error", err)
				return
			}
		}
	}
}

// SendProgress transmits a progress frame upstream.
func (t *TelemetryClient) SendProgress(p ProgressPayload) error {
	return t.send("progress", p)
}

// SendLog transmits a log frame upstream.
func (t *TelemetryClient) SendLog(l LogPayload) error {
	return t.send("log", l)
}

// SendNotification transmits a notification frame upstream.
func (t *TelemetryClient) SendNotification(n NotificationPayload) error {
	return t.send("notification", n)
}

func (t *TelemetryClient) send(frameType string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling %s payload: %w", frameType, err)
	}
	frame := TelemetryFrame{
		Type:    frameType,
		Payload: json.RawMessage(data),
	}

	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()

	if conn == nil {
		t.logger.Warn("upstream frame dropped: websocket not connected",
			"frame_type", frameType,
			"payload_size", len(data),
		)
		return fmt.Errorf("upstream websocket not connected")
	}

	if err := t.writeFrame(conn, frame); err != nil {
		t.logger.Warn("upstream frame write failed",
			"frame_type", frameType,
			"error", err,
		)
		return err
	}

	t.logger.Debug("upstream frame sent", "frame_type", frameType, "payload_size", len(data))
	return nil
}

func (t *TelemetryClient) writeFrame(conn *websocket.Conn, frame TelemetryFrame) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}
	return conn.WriteJSON(frame)
}

func (t *TelemetryClient) wsURL() string {
	base := t.serverURL
	base = strings.Replace(base, "https://", "wss://", 1)
	base = strings.Replace(base, "http://", "ws://", 1)
	return fmt.Sprintf("%s%s?addon_id=%d", base, wsPath, t.addonID)
}
