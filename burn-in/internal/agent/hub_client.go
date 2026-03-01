package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"runtime"
	"time"
)

const (
	registerPath  = "/api/agents/register"
	backoffBase   = 2 * time.Second
	backoffMax    = 60 * time.Second
	backoffFactor = 2.0
)

// RegistrationPayload is the body sent to POST /api/agents/register.
type RegistrationPayload struct {
	AgentID       string      `json:"agent_id"`
	Hostname      string      `json:"hostname"`
	Arch          string      `json:"arch"`
	AdvertiseAddr string      `json:"advertise_addr"`
	Drives        []DriveInfo `json:"drives"`
}

// HubClient handles registration and communication with the burn-in hub.
type HubClient struct {
	hubURL     string
	psk        string
	agentID    string
	advAddr    string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewHubClient creates a client for communicating with the hub.
func NewHubClient(hubURL, psk, agentID, advertiseAddr string, logger *slog.Logger) *HubClient {
	return &HubClient{
		hubURL:     hubURL,
		psk:        psk,
		agentID:    agentID,
		advAddr:    advertiseAddr,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
	}
}

// Register discovers local drives and sends the registration payload to the
// hub with exponential backoff. It blocks until registration succeeds or the
// context is cancelled.
func (c *HubClient) Register(ctx context.Context) error {
	drives, err := DiscoverDrives(c.logger)
	if err != nil {
		c.logger.Warn("drive discovery failed, registering with empty drive list", "error", err)
		drives = nil
	}

	hostname, _ := os.Hostname()

	payload := RegistrationPayload{
		AgentID:       c.agentID,
		Hostname:      hostname,
		Arch:          runtime.GOARCH,
		AdvertiseAddr: c.advAddr,
		Drives:        drives,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling registration payload: %w", err)
	}

	var attempt int
	for {
		err := c.doRegister(ctx, body)
		if err == nil {
			c.logger.Info("registered with hub",
				"agent_id", c.agentID,
				"hostname", hostname,
				"drives", len(drives),
			)
			return nil
		}

		attempt++
		delay := backoffDelay(attempt)
		c.logger.Warn("hub registration failed, retrying",
			"error", err,
			"attempt", attempt,
			"retry_in", delay,
		)

		select {
		case <-ctx.Done():
			return fmt.Errorf("registration cancelled: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
}

func (c *HubClient) doRegister(ctx context.Context, body []byte) error {
	url := c.hubURL + registerPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.psk)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", registerPath, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s returned %d: %s", registerPath, resp.StatusCode, string(respBody))
	}

	return nil
}

func backoffDelay(attempt int) time.Duration {
	delay := float64(backoffBase) * math.Pow(backoffFactor, float64(attempt-1))
	if delay > float64(backoffMax) {
		delay = float64(backoffMax)
	}
	return time.Duration(delay)
}
