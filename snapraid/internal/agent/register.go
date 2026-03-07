package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// RegisterRequest is the payload sent to the Hub's /api/agents/register endpoint.
type RegisterRequest struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	Version  string `json:"version"`
}

// RegisterWithHub announces this agent to the Hub. It retries with exponential
// backoff until the context is cancelled or registration succeeds.
func RegisterWithHub(ctx context.Context, hubURL, agentID, hostname, version string, port int, logger *slog.Logger) {
	addr := fmt.Sprintf("http://%s:%d", hostname, port)

	req := RegisterRequest{
		ID:       agentID,
		Hostname: hostname,
		Address:  addr,
		Version:  version,
	}

	body, _ := json.Marshal(req)
	url := hubURL + "/api/agents/register"

	backoff := 2 * time.Second
	maxBackoff := 60 * time.Second

	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			logger.Error("failed to create registration request", "error", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(httpReq)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				logger.Info("registered with hub", "hub_url", hubURL, "agent_id", agentID)
				return
			}
			logger.Warn("hub registration returned non-OK", "status", resp.StatusCode)
		} else {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("hub registration failed, retrying", "error", err, "backoff", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
