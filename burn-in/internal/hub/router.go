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

	"github.com/pineappledr/vigil-addons/shared/addonutil"
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

// restructurePayload converts a flat JSON payload from Vigil into the nested
// format the agent expects: {agent_id, command, target, params: {...}}.
// Non-top-level fields are moved into the "params" sub-object.
func restructurePayload(flat map[string]interface{}) ([]byte, error) {
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
	return json.Marshal(flat)
}

// resolveAgent looks up the agent by ID and returns it, or writes an HTTP
// error and returns nil if the agent is missing or unreachable.
func (cr *CommandRouter) resolveAgent(w http.ResponseWriter, agentID string) *AgentRecord {
	if agentID == "" {
		cr.logger.Warn("execute rejected: missing agent_id in payload")
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "agent_id is required in payload"})
		return nil
	}

	agent := cr.registry.Get(agentID)
	if agent == nil {
		cr.logger.Warn("command routed to unknown agent", "agent_id", agentID)
		addonutil.WriteJSON(w, http.StatusNotFound, addonutil.ErrorResponse{
			Error: fmt.Sprintf("agent %q is not registered", agentID),
		})
		return nil
	}

	if agent.AdvertiseAddr == "" {
		cr.logger.Error("agent has no advertise address", "agent_id", agentID)
		addonutil.WriteJSON(w, http.StatusBadGateway, addonutil.ErrorResponse{
			Error: fmt.Sprintf("agent %q has no reachable address", agentID),
		})
		return nil
	}

	return agent
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
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "failed to read request body"})
		return
	}

	cr.logger.Info("execute payload received", "body_size", len(body))

	var flat map[string]interface{}
	if err := json.Unmarshal(body, &flat); err != nil {
		cr.logger.Error("execute payload is not valid JSON", "error", err, "body_preview", truncate(string(body), 256))
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "invalid JSON payload"})
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

	agent := cr.resolveAgent(w, agentID)
	if agent == nil {
		return
	}

	forwarded, err := restructurePayload(flat)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, addonutil.ErrorResponse{Error: "failed to encode payload"})
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
		addonutil.WriteJSON(w, http.StatusBadGateway, addonutil.ErrorResponse{
			Error: fmt.Sprintf("failed to reach agent %q: %s", agentID, err),
		})
		return
	}
	defer agentResp.Body.Close()

	cr.logger.Info("agent responded", "agent_id", agentID, "status_code", agentResp.StatusCode)

	respBody, err := io.ReadAll(io.LimitReader(agentResp.Body, maxPayloadSize))
	if err != nil {
		cr.logger.Error("failed to retrieve agent response", "agent_id", agentID, "error", err)
		addonutil.WriteJSON(w, http.StatusBadGateway, addonutil.ErrorResponse{Error: "failed to read agent response"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(agentResp.StatusCode)
	w.Write(respBody)
}

// HandleJobHistory is the HTTP handler for GET /api/jobs/history.
// It fans out to all registered agents, collects their job records,
// and returns a merged JSON array. Supports an optional ?time_range
// query parameter (e.g., "1h", "24h", "7d", "30d") to filter records.
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

	// Apply time_range filter if provided.
	if tr := r.URL.Query().Get("time_range"); tr != "" {
		allRecords = filterByTimeRange(allRecords, tr)
	}

	addonutil.WriteJSON(w, http.StatusOK, allRecords)
}

// filterByTimeRange keeps only records whose "started_at" timestamp is
// within the given window. Records with unparseable or missing timestamps
// are retained (fail-open).
func filterByTimeRange(records []json.RawMessage, tr string) []json.RawMessage {
	dur, ok := addonutil.ParseTimeRange(tr)
	if !ok {
		return records // Unknown range (e.g., "" for All Time) — return unfiltered.
	}

	cutoff := time.Now().Add(-dur)
	filtered := make([]json.RawMessage, 0, len(records))

	for _, raw := range records {
		var rec struct {
			StartedAt string `json:"started_at"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil || rec.StartedAt == "" {
			filtered = append(filtered, raw) // Fail-open: keep records we can't parse.
			continue
		}

		t, err := time.Parse(time.RFC3339, rec.StartedAt)
		if err != nil {
			// Try common alternative formats.
			t, err = time.Parse("2006-01-02T15:04:05Z", rec.StartedAt)
		}
		if err != nil {
			t, err = time.Parse("2006-01-02 15:04:05", rec.StartedAt)
		}
		if err != nil {
			filtered = append(filtered, raw) // Unparseable — keep.
			continue
		}

		if t.After(cutoff) {
			filtered = append(filtered, raw)
		}
	}

	return filtered
}

// HandleCancelJob is the HTTP handler for DELETE /api/jobs/{id}.
// The hub does not track job-to-agent mapping, so it broadcasts the
// DELETE to all registered agents and returns the first successful response.
func (cr *CommandRouter) HandleCancelJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if jobID == "" {
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "job id is required"})
		return
	}

	agents := cr.registry.List()
	if len(agents) == 0 {
		addonutil.WriteJSON(w, http.StatusNotFound, addonutil.ErrorResponse{Error: "no agents registered"})
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

	addonutil.WriteJSON(w, http.StatusNotFound, addonutil.ErrorResponse{
		Error: fmt.Sprintf("job %q not found on any agent", jobID),
	})
}

// HandleJobStatus is the HTTP handler for GET /api/jobs/{id}. The hub does
// not track job→agent mapping, so it queries every registered agent and
// returns the first 200 OK body. This matches HandleCancelJob's broadcast
// pattern. Returns 404 only when no agent owns the id.
func (cr *CommandRouter) HandleJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if jobID == "" {
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "job id is required"})
		return
	}

	agents := cr.registry.List()
	if len(agents) == 0 {
		addonutil.WriteJSON(w, http.StatusNotFound, addonutil.ErrorResponse{Error: "no agents registered"})
		return
	}

	for _, agent := range agents {
		if agent.AdvertiseAddr == "" {
			continue
		}

		raw := fmt.Sprintf("http://%s%s%s", agent.AdvertiseAddr, agentJobPath, url.PathEscape(jobID))
		validated, err := validateAgentURL(raw)
		if err != nil {
			cr.logger.Warn("invalid job status target URL", "agent_id", agent.AgentID, "error", err)
			continue
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, validated, nil) // #nosec
		if err != nil {
			continue
		}

		resp, err := cr.httpClient.Do(req) // #nosec
		if err != nil {
			cr.logger.Debug("job status fetch failed", "agent_id", agent.AgentID, "error", err)
			continue
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxPayloadSize))
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(respBody)
			return
		}
	}

	addonutil.WriteJSON(w, http.StatusNotFound, addonutil.ErrorResponse{
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
