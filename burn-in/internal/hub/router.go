package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	agentForwardTimeout = 30 * time.Second
	maxPayloadSize      = 1 << 20 // 1 MB
	agentExecutePath    = "/api/execute"
)

// CommandRouter forwards signed execution payloads from the Vigil server
// to the correct worker agent based on agent_id.
type CommandRouter struct {
	registry   *AgentRegistry
	httpClient *http.Client
	logger     *slog.Logger
}

// NewCommandRouter creates a router that resolves agents via the registry.
func NewCommandRouter(registry *AgentRegistry, logger *slog.Logger) *CommandRouter {
	return &CommandRouter{
		registry: registry,
		httpClient: &http.Client{
			Timeout: agentForwardTimeout,
		},
		logger: logger,
	}
}

// executePayloadHeader is used only to extract the agent_id for routing.
// The full signed payload is forwarded untouched.
type executePayloadHeader struct {
	AgentID string `json:"agent_id"`
}

// HandleExecute is the HTTP handler for POST /api/execute.
// It reads the agent_id from the payload, looks up the agent, and forwards
// the complete signed payload to the agent's API.
func (cr *CommandRouter) HandleExecute(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadSize))
	if err != nil {
		cr.logger.Error("failed to read execute payload", "error", err)
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "failed to read request body"})
		return
	}

	var header executePayloadHeader
	if err := json.Unmarshal(body, &header); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON payload"})
		return
	}
	if header.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "agent_id is required in payload"})
		return
	}

	agent := cr.registry.Get(header.AgentID)
	if agent == nil {
		cr.logger.Warn("command routed to unknown agent", "agent_id", header.AgentID)
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("agent %q is not registered", header.AgentID),
		})
		return
	}

	if agent.AdvertiseAddr == "" {
		cr.logger.Error("agent has no advertise address", "agent_id", header.AgentID)
		writeJSON(w, http.StatusBadGateway, errorResponse{
			Error: fmt.Sprintf("agent %q has no reachable address", header.AgentID),
		})
		return
	}

	cr.logger.Info("routing command to agent",
		"agent_id", header.AgentID,
		"target", agent.AdvertiseAddr,
	)

	agentResp, err := cr.forwardToAgent(r.Context(), agent.AdvertiseAddr, body)
	if err != nil {
		cr.logger.Error("failed to transmit command to agent",
			"agent_id", header.AgentID,
			"error", err,
		)
		writeJSON(w, http.StatusBadGateway, errorResponse{
			Error: fmt.Sprintf("failed to reach agent %q: %s", header.AgentID, err),
		})
		return
	}
	defer agentResp.Body.Close()

	// Relay the agent's response back to the caller.
	respBody, err := io.ReadAll(io.LimitReader(agentResp.Body, maxPayloadSize))
	if err != nil {
		cr.logger.Error("failed to retrieve agent response", "agent_id", header.AgentID, "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "failed to read agent response"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(agentResp.StatusCode)
	w.Write(respBody)
}

func (cr *CommandRouter) forwardToAgent(ctx context.Context, advertiseAddr string, payload []byte) (*http.Response, error) {
	url := fmt.Sprintf("http://%s%s", advertiseAddr, agentExecutePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return cr.httpClient.Do(req)
}
