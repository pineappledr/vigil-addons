package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
)

// Server is the Hub HTTP server exposing registry, command, and telemetry endpoints.
type Server struct {
	cfg        *config.HubConfig
	registry   *Registry
	aggregator *Aggregator
	router     *CommandRouter
	mux        *http.ServeMux
	logger     *slog.Logger
}

// NewServer creates the Hub server with all dependencies.
func NewServer(cfg *config.HubConfig, registry *Registry, aggregator *Aggregator, router *CommandRouter, logger *slog.Logger) *Server {
	s := &Server{
		cfg:        cfg,
		registry:   registry,
		aggregator: aggregator,
		router:     router,
		mux:        http.NewServeMux(),
		logger:     logger,
	}
	s.routes()
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/deploy-info", s.handleDeployInfo)
	s.mux.HandleFunc("POST /api/agents/register", s.handleAgentRegister)
	s.mux.HandleFunc("GET /api/agents", s.handleAgentList)
	s.mux.HandleFunc("POST /api/command", s.handleCommand)
	s.mux.HandleFunc("POST /api/telemetry/ingest", s.handleTelemetryIngest)
	s.mux.HandleFunc("POST /api/config/{agentID}", s.handleConfigForward)
	s.mux.HandleFunc("POST /api/config", s.handleConfigFromBody)
	s.mux.HandleFunc("GET /api/config", s.handleGetConfig)
	s.mux.HandleFunc("DELETE /api/agents/{id}", s.handleAgentDelete)
	s.mux.HandleFunc("GET /api/jobs/history", s.handleProxyToAgent)
	s.mux.HandleFunc("GET /api/logs/history", s.handleProxyToAgent)
	s.mux.HandleFunc("GET /api/disk_status", s.handleTelemetryField)
	s.mux.HandleFunc("GET /api/smart_status", s.handleTelemetryField)
	s.mux.HandleFunc("GET /api/active_job", s.handleTelemetryField)
	s.mux.HandleFunc("GET /api/jobs/active", s.handleActiveJobs)
	s.mux.HandleFunc("POST /api/rotate-token", s.handleRotateToken)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"status":"ok"}`)
}

// handleDeployInfo returns connection details that the deploy-wizard
// prefills into the agent docker-compose template.
func (s *Server) handleDeployInfo(w http.ResponseWriter, r *http.Request) {
	writeHubJSON(w, http.StatusOK, map[string]string{
		"hub_url":   fmt.Sprintf("http://%s:%d", r.Host, s.cfg.Listen.Port),
		"hub_token": s.cfg.Vigil.Token,
	})
}

// AgentRegisterRequest is the payload from Agent self-registration.
type AgentRegisterRequest struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	Version  string `json:"version"`
}

func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var req AgentRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	entry := AgentEntry{
		ID:       req.ID,
		Hostname: req.Hostname,
		Address:  req.Address,
		Version:  req.Version,
	}

	if err := s.registry.Register(entry); err != nil {
		s.logger.Error("failed to register agent", "agent_id", req.ID, "error", err)
		writeHubJSON(w, http.StatusInternalServerError, map[string]string{"error": "registration failed"})
		return
	}

	s.logger.Info("agent registered", "agent_id", req.ID, "hostname", req.Hostname, "address", req.Address)
	writeHubJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	writeHubJSON(w, http.StatusOK, s.registry.ListViews())
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent id"})
		return
	}

	if !s.registry.Delete(agentID) {
		writeHubJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}

	s.logger.Info("agent deleted", "agent_id", agentID)
	writeHubJSON(w, http.StatusOK, map[string]string{"status": "deleted", "agent_id": agentID})
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	// Accept both {"action":"status"} and {"command":"status"} since the
	// Vigil action proxy forwards form data as-is (which uses "command").
	var raw struct {
		AgentID string          `json:"agent_id"`
		Action  string          `json:"action"`
		Command string          `json:"command"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid command"})
		return
	}

	cmd := CommandMessage{
		AgentID: raw.AgentID,
		Action:  raw.Action,
		Params:  raw.Params,
	}
	if cmd.Action == "" {
		cmd.Action = raw.Command
	}

	resp, err := s.router.RouteCommand(cmd)
	if err != nil {
		s.logger.Error("command routing failed", "agent_id", cmd.AgentID, "error", err)

		// Emit a job_failed notification upstream.
		s.aggregator.emitCommandFailure(cmd.AgentID, cmd.Action, err)

		writeHubJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

// TelemetryIngestRequest carries a raw telemetry frame from an Agent.
type TelemetryIngestRequest struct {
	AgentID string          `json:"agent_id"`
	Payload json.RawMessage `json:"payload"`
}

func (s *Server) handleTelemetryIngest(w http.ResponseWriter, r *http.Request) {
	var req TelemetryIngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid telemetry"})
		return
	}

	s.aggregator.IngestAgentFrame(req.AgentID, req.Payload)
	writeHubJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

// handleConfigFromBody handles POST /api/config where agent_id is in the JSON body.
// This is the path used by the Vigil action proxy (which sends to /api/{action}).
// The proxy sends flat form data: {"agent_id":"x", "key1":"val1", ...}
// The agent expects: {"values": {"key1":"val1", ...}}
func (s *Server) handleConfigFromBody(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}

	var flat map[string]interface{}
	if err := json.Unmarshal(body, &flat); err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	agentID, _ := flat["agent_id"].(string)
	if agentID == "" {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent_id in body"})
		return
	}

	// Restructure: extract config values (everything except agent_id)
	values := make(map[string]string, len(flat)-1)
	for k, v := range flat {
		if k == "agent_id" {
			continue
		}
		values[k] = fmt.Sprintf("%v", v)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"values": values,
	})

	s.logger.Info("forwarding config update", "agent_id", agentID, "keys", len(values))

	if err := s.router.RouteConfigUpdate(agentID, payload); err != nil {
		s.logger.Error("config forward failed", "agent_id", agentID, "error", err)
		writeHubJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	writeHubJSON(w, http.StatusOK, map[string]string{"status": "forwarded"})
}

func (s *Server) handleConfigForward(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	if agentID == "" {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent_id"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}

	if err := s.router.RouteConfigUpdate(agentID, body); err != nil {
		s.logger.Error("config forward failed", "agent_id", agentID, "error", err)
		writeHubJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	writeHubJSON(w, http.StatusOK, map[string]string{"status": "forwarded"})
}


// handleGetConfig proxies GET /api/config?agent_id=xxx to the target agent.
// If no agent_id is given, it returns config from the first online agent.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")

	if agentID == "" {
		// Default to first online agent
		views := s.registry.ListViews()
		for _, v := range views {
			if v.Status == "online" {
				agentID = v.ID
				break
			}
		}
		if agentID == "" {
			writeHubJSON(w, http.StatusNotFound, map[string]string{"error": "no online agents"})
			return
		}
	}

	body, err := s.router.FetchAgentConfig(agentID)
	if err != nil {
		s.logger.Error("failed to fetch agent config", "agent_id", agentID, "error", err)
		writeHubJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	// The Vigil proxy sends form data as {"data": {"confirm": "ROTATE"}}
	var req struct {
		Data struct {
			Confirm string `json:"confirm"`
		} `json:"data"`
		// Direct call (not via proxy)
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	confirm := req.Data.Confirm
	if confirm == "" {
		confirm = req.Confirm
	}
	if confirm != "ROTATE" {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "type ROTATE to confirm"})
		return
	}

	newToken, err := GenerateToken()
	if err != nil {
		s.logger.Error("failed to generate new token", "error", err)
		writeHubJSON(w, http.StatusInternalServerError, map[string]string{"error": "token generation failed"})
		return
	}

	if err := PersistToken(s.cfg.Data.RegistryPath, newToken); err != nil {
		s.logger.Error("failed to persist new token", "error", err)
		writeHubJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save token"})
		return
	}

	s.cfg.Vigil.Token = newToken
	s.logger.Info("hub token rotated successfully")
	writeHubJSON(w, http.StatusOK, map[string]string{
		"status":    "rotated",
		"hub_token": newToken,
	})
}

// handleProxyToAgent forwards a GET request to the first online agent,
// preserving the request path and query string. Used for /api/jobs/history
// and /api/logs/history.
func (s *Server) handleProxyToAgent(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")

	if agentID == "" {
		views := s.registry.ListViews()
		for _, v := range views {
			if v.Status == "online" {
				agentID = v.ID
				break
			}
		}
		if agentID == "" {
			writeHubJSON(w, http.StatusNotFound, map[string]string{"error": "no online agents"})
			return
		}
	}

	// Build the path + query to forward.
	pathAndQuery := r.URL.Path
	if r.URL.RawQuery != "" {
		pathAndQuery += "?" + r.URL.RawQuery
	}

	body, statusCode, err := s.router.ProxyGet(agentID, pathAndQuery)
	if err != nil {
		s.logger.Error("proxy to agent failed", "agent_id", agentID, "error", err)
		writeHubJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	w.Write(body)
}

// handleTelemetryField serves cached telemetry data from the aggregator.
// The API path determines which field is returned:
//
//	/api/disk_status  → array_status (contains .disks array)
//	/api/smart_status → smart_status (contains .disks array)
//	/api/active_job   → active_job
func (s *Server) handleTelemetryField(w http.ResponseWriter, r *http.Request) {
	// Map URL path to telemetry payload field name.
	fieldMap := map[string]string{
		"/api/disk_status":  "array_status",
		"/api/smart_status": "smart_status",
		"/api/active_job":   "active_job",
	}

	field, ok := fieldMap[r.URL.Path]
	if !ok {
		writeHubJSON(w, http.StatusNotFound, map[string]string{"error": "unknown telemetry field"})
		return
	}

	agentID := r.URL.Query().Get("agent_id")
	data := s.aggregator.LatestTelemetryField(agentID, field)
	if data == nil {
		// Return empty array/null so the frontend shows "No data" instead of error.
		w.Header().Set("Content-Type", "application/json")
		if field == "active_job" {
			w.Write([]byte("null"))
		} else {
			w.Write([]byte("[]"))
		}
		return
	}

	// For disk_status and smart_status, the frontend expects an array of disks.
	// StatusReport uses "disk_status" key, SmartReport uses "disks" key.
	if field == "array_status" || field == "smart_status" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err == nil {
			// Try both key names: SmartReport → "disks", StatusReport → "disk_status"
			for _, key := range []string{"disks", "disk_status"} {
				if arr, exists := obj[key]; exists {
					w.Header().Set("Content-Type", "application/json")
					w.Write(arr)
					return
				}
			}
		}
		// Fallback: return as-is
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleActiveJobs returns the current active job (if any) as a JSON array
// for the progress component's initial fetch.
func (s *Server) handleActiveJobs(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	data := s.aggregator.LatestTelemetryField(agentID, "active_job")
	if data == nil || string(data) == "null" {
		writeHubJSON(w, http.StatusOK, []any{})
		return
	}
	// Wrap single job in array for the progress component.
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("["))
	w.Write(data)
	w.Write([]byte("]"))
}

func writeHubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
