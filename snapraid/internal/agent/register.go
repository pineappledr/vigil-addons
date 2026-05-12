package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// RegisterRequest is the payload sent to the Hub's /api/agents/register endpoint.
type RegisterRequest struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	Version  string `json:"version"`
}

// pskHint returns a short, non-secret identifier for a PSK: the first and last
// four characters. Used in diagnostic logs so a mismatch can be traced without
// leaking the key.
func pskHint(psk string) string {
	if len(psk) < 8 {
		return "<empty or too short>"
	}
	return psk[:4] + "…" + psk[len(psk)-4:]
}

// RegisterWithHub announces this agent to the Hub. It retries with exponential
// backoff until the context is cancelled or registration succeeds.
func RegisterWithHub(ctx context.Context, hubURL, psk, agentID, hostname, advertiseAddr, version string, logger *slog.Logger) {
	req := RegisterRequest{
		ID:       agentID,
		Hostname: hostname,
		Address:  advertiseAddr,
		Version:  version,
	}

	body, _ := json.Marshal(req)
	url := hubURL + "/api/agents/register"

	backoff := 2 * time.Second
	maxBackoff := 60 * time.Second
	authFailures := 0

	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			logger.Error("failed to create registration request", "error", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+psk)

		resp, err := http.DefaultClient.Do(httpReq)
		if err == nil {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				logger.Info("registered with hub", "hub_url", hubURL, "agent_id", agentID)
				return
			}

			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				authFailures++
				// Surface the actionable diagnosis loudly on the first failure,
				// then keep it terse so we don't spam the log on every retry.
				if authFailures == 1 {
					logger.Error("hub rejected registration: pre-shared key mismatch — "+
						"this agent's hub_psk does not match the hub's PSK. "+
						"Re-run the SnapRAID deploy wizard, or copy the value from the hub's PSK file "+
						"(shown in the hub log as psk_path) into this agent's config (hub.psk / HUB_PSK).",
						"hub_url", hubURL,
						"status", resp.StatusCode,
						"configured_psk", pskHint(psk),
						"hub_response", strings.TrimSpace(string(respBody)))
				} else {
					logger.Warn("hub registration still rejected — PSK mismatch unresolved",
						"status", resp.StatusCode, "attempts", authFailures)
				}
			default:
				logger.Warn("hub registration returned non-OK",
					"status", resp.StatusCode, "hub_response", strings.TrimSpace(string(respBody)))
			}
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
