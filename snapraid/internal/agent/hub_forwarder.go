package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// TelemetryIngestRequest is the payload for the Hub's /api/telemetry/ingest endpoint.
type TelemetryIngestRequest struct {
	AgentID string          `json:"agent_id"`
	Payload json.RawMessage `json:"payload"`
}

// LogIngestRequest is the payload for the Hub's /api/logs/ingest endpoint.
type LogIngestRequest struct {
	AgentID string  `json:"agent_id"`
	Log     LogLine `json:"log"`
}

// SetupLogForwarding installs a LogSink on the Collector that POSTs each log
// line to the Hub's /api/logs/ingest endpoint for real-time display.
func SetupLogForwarding(ctx context.Context, collector *Collector, hubURL, agentID string, logger *slog.Logger) {
	logIngestURL := hubURL + "/api/logs/ingest"

	collector.SetLogSink(func(line LogLine) {
		body, _ := json.Marshal(LogIngestRequest{
			AgentID: agentID,
			Log:     line,
		})

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, logIngestURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return // silently drop — log forwarding is best-effort
		}
		resp.Body.Close()
	})
}

// StartHubForwarder runs the telemetry loop and forwards each frame to the Hub
// via POST /api/telemetry/ingest. This keeps the agent's LastSeenAt updated
// so the Hub can report online/offline status.
//
// In addition to the periodic ticker, it listens on the Collector's flush
// channel for immediate sends (e.g. when a job starts or finishes).
func StartHubForwarder(ctx context.Context, collector *Collector, hubURL, agentID string, interval time.Duration, logger *slog.Logger) {
	ingestURL := hubURL + "/api/telemetry/ingest"

	logger.Info("hub telemetry forwarder started", "hub_url", hubURL, "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	flushCh := collector.FlushCh()

	send := func() {
		payload, err := collector.MarshalPayload()
		if err != nil {
			logger.Error("failed to marshal telemetry", "error", err)
			return
		}

		body, _ := json.Marshal(TelemetryIngestRequest{
			AgentID: agentID,
			Payload: payload,
		})

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ingestURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("hub telemetry forward failed", "error", err)
			}
			return
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			logger.Warn("hub telemetry forward returned non-OK", "status", resp.StatusCode)
		}
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("hub telemetry forwarder stopped")
			return
		case <-ticker.C:
			send()
		case <-flushCh:
			send()
			// Reset ticker so the next periodic send is a full interval away.
			ticker.Reset(interval)
		}
	}
}
