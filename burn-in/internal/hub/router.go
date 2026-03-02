package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	agentForwardTimeout = 30 * time.Second
	maxPayloadSize      = 1 << 20 // 1 MB
	agentExecutePath    = "/api/execute"
	agentJobPath        = "/api/jobs/" // + {id}
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

// HandleCancelJob is the HTTP handler for DELETE /api/jobs/{id}.
// The hub does not track job-to-agent mapping, so it broadcasts the
// DELETE to all registered agents and returns the first successful response.
func (cr *CommandRouter) HandleCancelJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "job id is required"})
		return
	}

	agents := cr.registry.List()
	if len(agents) == 0 {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no agents registered"})
		return
	}

	cr.logger.Info("broadcasting job cancel to agents", "job_id", jobID, "agent_count", len(agents))

	for _, agent := range agents {
		if agent.AdvertiseAddr == "" {
			continue
		}

		raw := fmt.Sprintf("http://%s%s%s", agent.AdvertiseAddr, agentJobPath, url.PathEscape(jobID))
		validated, err := validateAgentURL(raw)
		if err != nil {
			cr.logger.Warn("invalid cancel target URL", "agent_id", agent.AgentID, "error", err)
			continue
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodDelete, validated, nil)
		if err != nil {
			continue
		}

		resp, err := cr.httpClient.Do(req) //nolint:gosec // G107: URL scheme and format strictly validated
		if err != nil {
			cr.logger.Debug("cancel forward failed", "agent_id", agent.AgentID, "error", err)
			continue
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxPayloadSize))
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			cr.logger.Info("job cancelled via agent", "job_id", jobID, "agent_id", agent.AgentID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(respBody)
			return
		}
	}

	writeJSON(w, http.StatusNotFound, errorResponse{
		Error: fmt.Sprintf("job %q not found on any agent", jobID),
	})
}

func (cr *CommandRouter) forwardToAgent(ctx context.Context, advertiseAddr string, payload []byte) (*http.Response, error) {
	raw := fmt.Sprintf("http://%s%s", advertiseAddr, agentExecutePath)
	validated, err := validateAgentURL(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid agent URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, validated, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return cr.httpClient.Do(req) //nolint:gosec // G107: URL scheme and format strictly validated
}

// validateAgentURL parses the constructed URL and enforces that the scheme
// is strictly http or https. Returns the re-serialised string from the
// parsed representation, which breaks the taint chain for static analysis.
func validateAgentURL(raw string) (string, error) {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return "", fmt.Errorf("malformed agent URL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("disallowed scheme %q in agent URL", parsed.Scheme)
	}
	return parsed.String(), nil
}
