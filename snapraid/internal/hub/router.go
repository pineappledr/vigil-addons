package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// CommandMessage is a command received from the Vigil Server to forward to an Agent.
type CommandMessage struct {
	AgentID string          `json:"agent_id"`
	Action  string          `json:"action"`
	Params  json.RawMessage `json:"params"`
}

// CommandRouter resolves target Agents and forwards commands via HTTP POST.
type CommandRouter struct {
	registry *Registry
	client   *http.Client
	logger   *slog.Logger
}

// NewCommandRouter creates a CommandRouter wired to the Agent registry.
func NewCommandRouter(registry *Registry, logger *slog.Logger) *CommandRouter {
	return &CommandRouter{
		registry: registry,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}
}

// RouteCommand resolves the target Agent and forwards the command as POST /api/execute.
func (cr *CommandRouter) RouteCommand(cmd CommandMessage) ([]byte, error) {
	agent := cr.registry.Get(cmd.AgentID)
	if agent == nil {
		return nil, fmt.Errorf("agent %s not found in registry", cmd.AgentID)
	}

	// Build the execute request payload
	payload := map[string]any{
		"command": cmd.Action,
	}
	if cmd.Params != nil {
		var extra map[string]any
		if err := json.Unmarshal(cmd.Params, &extra); err == nil {
			for k, v := range extra {
				payload[k] = v
			}
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal command: %w", err)
	}

	url := fmt.Sprintf("%s/api/execute", agent.Address)
	cr.logger.Info("routing command to agent", "agent_id", cmd.AgentID, "url", url, "action", cmd.Action)

	resp, err := cr.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("forward to agent %s: %w", cmd.AgentID, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read agent response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return respBody, fmt.Errorf("agent returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// RouteConfigUpdate forwards a config update to the target Agent via POST /api/config.
func (cr *CommandRouter) RouteConfigUpdate(agentID string, configPayload []byte) error {
	agent := cr.registry.Get(agentID)
	if agent == nil {
		return fmt.Errorf("agent %s not found in registry", agentID)
	}

	url := fmt.Sprintf("%s/api/config", agent.Address)
	cr.logger.Info("routing config update to agent", "agent_id", agentID, "url", url)

	resp, err := cr.client.Post(url, "application/json", bytes.NewReader(configPayload))
	if err != nil {
		return fmt.Errorf("forward config to agent %s: %w", agentID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
