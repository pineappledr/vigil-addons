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

// TelemetryFrame is the envelope for all frames sent upstream to Vigil.
type TelemetryFrame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// TelemetryClient manages the persistent WebSocket connection to the Vigil server.
// It reads aggregated frames from the upstream channel and transmits them to Vigil.
type TelemetryClient struct {
	serverURL    string
	addonID      int64
	token        string
	heartbeatInt time.Duration
	upstream     <-chan []byte
	logger       *slog.Logger

	mu    sync.Mutex
	conn  *websocket.Conn
	ready chan struct{} // closed once the first connection succeeds
	once  sync.Once
}

// NewTelemetryClient creates an upstream telemetry client that reads from
// the provided channel and forwards frames to the Vigil server via WebSocket.
func NewTelemetryClient(serverURL string, addonID int64, token string, heartbeatInterval time.Duration, upstream <-chan []byte, logger *slog.Logger) *TelemetryClient {
	return &TelemetryClient{
		serverURL:    serverURL,
		addonID:      addonID,
		token:        token,
		heartbeatInt: heartbeatInterval,
		upstream:     upstream,
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
		delay := registrationBackoff(attempt)
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
	header.Set("Authorization", "Bearer "+t.token)

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
	conn.SetReadDeadline(time.Now().Add(pongWait))
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

	// Heartbeat + upstream forwarding in a separate goroutine.
	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()

	go func() {
		t.forwardLoop(serveCtx, conn)
		conn.Close()
	}()

	// Read loop keeps the connection alive and detects closure.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return fmt.Errorf("read: %w", err)
		}
	}
}

// forwardLoop reads from the upstream channel and sends frames, interleaved
// with periodic heartbeats.
func (t *TelemetryClient) forwardLoop(ctx context.Context, conn *websocket.Conn) {
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

		case data, ok := <-t.upstream:
			if !ok {
				return
			}
			// Wrap the raw aggregated frame as a telemetry payload.
			frame := TelemetryFrame{
				Type:    "telemetry",
				Payload: json.RawMessage(data),
			}
			if err := t.writeFrame(conn, frame); err != nil {
				t.logger.Warn("upstream frame write failed", "error", err)
				return
			}
			t.logger.Debug("upstream frame sent", "payload_size", len(data))
		}
	}
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
