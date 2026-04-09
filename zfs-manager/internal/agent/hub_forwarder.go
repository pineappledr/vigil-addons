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

// StartHubForwarder runs the telemetry loop and forwards each frame to the Hub
// via POST /api/telemetry/ingest. This keeps the agent's LastSeenAt updated
// so the Hub can report online/offline status.
//
// In addition to the periodic ticker, it listens on the Collector's flush
// channel for immediate sends (e.g. after a write operation).
func StartHubForwarder(ctx context.Context, collector *Collector, hubURL, psk, agentID string, interval time.Duration, logger *slog.Logger) {
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
		req.Header.Set("Authorization", "Bearer "+psk)

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
			ticker.Reset(interval)
		}
	}
}
