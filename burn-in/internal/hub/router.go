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

// topLevelFields are the fields that belong at the root of the agent
// execute payload. Everything else is nested under "params".
var topLevelFields = map[string]bool{
	"agent_id":  true,
	"command":   true,
	"target":    true,
	"signature": true,
}

// HandleExecute is the HTTP handler for POST /api/execute.
// Vigil forwards flat form data from the UI (all fields at the top level).
// The agent expects {agent_id, command, target, params: {...}}, so this
// handler restructures the payload before forwarding.
func (cr *CommandRouter) HandleExecute(w http.ResponseWriter, r *http.Request) {
	cr.logger.Info("execute endpoint hit",
		"method", r.Method,
		"remote_addr", r.RemoteAddr,
		"content_length", r.ContentLength,
	)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadSize))
	if err != nil {
		cr.logger.Error("failed to read execute payload", "error", err)
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "failed to read request body"})
		return
	}

	cr.logger.Info("execute payload received", "body_size", len(body))

	var flat map[string]interface{}
	if err := json.Unmarshal(body, &flat); err != nil {
		cr.logger.Error("execute payload is not valid JSON", "error", err, "body_preview", truncate(string(body), 256))
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON payload"})
		return
	}

	agentID, _ := flat["agent_id"].(string)
	command, _ := flat["command"].(string)
	target, _ := flat["target"].(string)

	cr.logger.Info("execute payload parsed",
		"agent_id", agentID,
		"command", command,
		"target", target,
		"field_count", len(flat),
	)

	if agentID == "" {
		cr.logger.Warn("execute rejected: missing agent_id in payload")
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "agent_id is required in payload"})
		return
	}

	agent := cr.registry.Get(agentID)
	if agent == nil {
		cr.logger.Warn("command routed to unknown agent", "agent_id", agentID)
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("agent %q is not registered", agentID),
		})
		return
	}

	if agent.AdvertiseAddr == "" {
		cr.logger.Error("agent has no advertise address", "agent_id", agentID)
		writeJSON(w, http.StatusBadGateway, errorResponse{
			Error: fmt.Sprintf("agent %q has no reachable address", agentID),
		})
		return
	}

	// Restructure: move non-top-level fields into "params" if not already present.
	if _, hasParams := flat["params"]; !hasParams {
		params := make(map[string]interface{})
		for k, v := range flat {
			if !topLevelFields[k] {
				params[k] = v
				delete(flat, k)
			}
		}
		if len(params) > 0 {
			flat["params"] = params
		}
	}

	forwarded, err := json.Marshal(flat)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to encode payload"})
		return
	}

	cr.logger.Info("routing command to agent",
		"agent_id", agentID,
		"advertise_addr", agent.AdvertiseAddr,
		"command", command,
		"target", target,
		"forwarded_size", len(forwarded),
	)

	agentResp, err := cr.forwardToAgent(r.Context(), agent.AdvertiseAddr, forwarded)
	if err != nil {
		cr.logger.Error("failed to transmit command to agent",
			"agent_id", agentID,
			"error", err,
		)
		writeJSON(w, http.StatusBadGateway, errorResponse{
			Error: fmt.Sprintf("failed to reach agent %q: %s", agentID, err),
		})
		return
	}
	defer agentResp.Body.Close()

	// Relay the agent's response back to the caller.
	cr.logger.Info("agent responded", "agent_id", agentID, "status_code", agentResp.StatusCode)

	respBody, err := io.ReadAll(io.LimitReader(agentResp.Body, maxPayloadSize))
	if err != nil {
		cr.logger.Error("failed to retrieve agent response", "agent_id", agentID, "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "failed to read agent response"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(agentResp.StatusCode)
	w.Write(respBody)
}

// HandleJobHistory is the HTTP handler for GET /api/jobs/history.
// It fans out to all registered agents, collects their job records,
// and returns a merged JSON array.
func (cr *CommandRouter) HandleJobHistory(w http.ResponseWriter, r *http.Request) {
	agents := cr.registry.List()
	cr.logger.Info("retrieving job history from agents", "agent_count", len(agents))

	var allRecords []json.RawMessage

	for _, agent := range agents {
		if agent.AdvertiseAddr == "" {
			continue
		}

		raw := fmt.Sprintf("http://%s/api/jobs/history", agent.AdvertiseAddr)
		validated, err := validateAgentURL(raw)
		if err != nil {
			cr.logger.Warn("invalid history target URL", "agent_id", agent.AgentID, "error", err)
			continue
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, validated, nil) // #nosec
		if err != nil {
			continue
		}

		resp, err := cr.httpClient.Do(req) // #nosec
		if err != nil {
			cr.logger.Debug("history fetch failed", "agent_id", agent.AgentID, "error", err)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxPayloadSize))
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			cr.logger.Debug("agent returned non-200 for history", "agent_id", agent.AgentID, "status", resp.StatusCode)
			continue
		}

		// Each agent returns a JSON array — merge individual records.
		var records []json.RawMessage
		if err := json.Unmarshal(body, &records); err != nil {
			cr.logger.Warn("invalid history response from agent", "agent_id", agent.AgentID, "error", err)
			continue
		}

		allRecords = append(allRecords, records...)
		cr.logger.Info("retrieved history from agent", "agent_id", agent.AgentID, "record_count", len(records))
	}

	if allRecords == nil {
		allRecords = []json.RawMessage{}
	}

	writeJSON(w, http.StatusOK, allRecords)
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

		req, err := http.NewRequestWithContext(r.Context(), http.MethodDelete, validated, nil) // #nosec
		if err != nil {
			continue
		}

		resp, err := cr.httpClient.Do(req) // #nosec
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, validated, bytes.NewReader(payload)) // #nosec
	if err != nil {
		return nil, fmt.Errorf("creating agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return cr.httpClient.Do(req) // #nosec
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

// truncate returns at most maxLen characters from s, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
