package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"
)

const (
	backoffBase    = 2 * time.Second
	backoffMax     = 60 * time.Second
	backoffFactor  = 2.0
	maxRetries     = 0 // 0 = unlimited retries
	registerPath   = "/api/addons/connect"
)

// VigilClient handles registration and communication with the Vigil server.
type VigilClient struct {
	serverURL  string
	agentToken string
	manifest   json.RawMessage
	httpClient *http.Client
	logger     *slog.Logger
}

// RegistrationResponse is the response from a successful addon registration.
type RegistrationResponse struct {
	AddonID   int64  `json:"addon_id"`
	SessionID string `json:"session_id"`
}

// NewVigilClient creates a client for communicating with the Vigil server.
// manifestData should be the raw bytes of the embedded manifest.json.
func NewVigilClient(serverURL, agentToken string, manifestData []byte, logger *slog.Logger) (*VigilClient, error) {
	if !json.Valid(manifestData) {
		return nil, fmt.Errorf("embedded manifest is not valid JSON")
	}

	return &VigilClient{
		serverURL:  serverURL,
		agentToken: agentToken,
		manifest:   json.RawMessage(manifestData),
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
	}, nil
}

// registrationPayload is the body sent to POST /api/addons.
type registrationPayload struct {
	Manifest json.RawMessage `json:"manifest"`
}

// Register sends the manifest to the Vigil server with exponential backoff.
// It blocks until registration succeeds or the context is cancelled.
func (c *VigilClient) Register(ctx context.Context) (*RegistrationResponse, error) {
	body, err := json.Marshal(registrationPayload{Manifest: c.manifest})
	if err != nil {
		return nil, fmt.Errorf("marshaling registration payload: %w", err)
	}

	var attempt int
	for {
		resp, err := c.doRegister(ctx, body)
		if err == nil {
			c.logger.Info("registered with vigil server",
				"addon_id", resp.AddonID,
				"session_id", resp.SessionID,
			)
			return resp, nil
		}

		attempt++
		delay := backoffDelay(attempt)
		c.logger.Warn("vigil registration failed, retrying",
			"error", err,
			"attempt", attempt,
			"retry_in", delay,
		)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("registration cancelled: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
}

func (c *VigilClient) doRegister(ctx context.Context, body []byte) (*RegistrationResponse, error) {
	url := c.serverURL + registerPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.agentToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", registerPath, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s returned %d: %s", registerPath, resp.StatusCode, string(respBody))
	}

	var result RegistrationResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

func backoffDelay(attempt int) time.Duration {
	delay := float64(backoffBase) * math.Pow(backoffFactor, float64(attempt-1))
	if delay > float64(backoffMax) {
		delay = float64(backoffMax)
	}
	return time.Duration(delay)
}
