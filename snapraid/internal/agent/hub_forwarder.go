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
func StartHubForwarder(ctx context.Context, collector *Collector, hubURL, agentID string, interval time.Duration, logger *slog.Logger) {
	ingestURL := hubURL + "/api/telemetry/ingest"

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			payload, err := collector.MarshalPayload()
			if err != nil {
				logger.Error("failed to marshal telemetry", "error", err)
				continue
			}

			body, _ := json.Marshal(TelemetryIngestRequest{
				AgentID: agentID,
				Payload: payload,
			})

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, ingestURL, bytes.NewReader(body))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				if ctx.Err() == nil {
					logger.Debug("hub telemetry forward failed", "error", err)
				}
				continue
			}
			resp.Body.Close()
		}
	}
}
