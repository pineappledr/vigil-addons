package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
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

// resolveAgentID returns the agent_id from the query string, falling back to
// the first online agent in the registry. Returns empty string if none found.
func (s *Server) resolveAgentID(r *http.Request) string {
	if id := r.URL.Query().Get("agent_id"); id != "" {
		return id
	}
	for _, v := range s.registry.ListViews() {
		if v.Status == "online" {
			return v.ID
		}
	}
	return ""
}

// decodeJSON decodes the request body into T and writes a 400 error response
// on failure. Returns the decoded value and true on success.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, errMsg string) (T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return v, false
	}
	return v, true
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
	s.mux.HandleFunc("GET /api/active_job", s.handleTelemetryField)
	s.mux.HandleFunc("GET /api/disk_storage", s.handleTelemetryField)
	s.mux.HandleFunc("GET /api/jobs/active", s.handleActiveJobs)
	s.mux.HandleFunc("DELETE /api/jobs/{id}", s.handleCancelJob)
	s.mux.HandleFunc("POST /api/rotate-token", s.handleRotateToken)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeRawJSON(w, []byte(`{"status":"ok"}`))
}

// handleDeployInfo returns connection details that the deploy-wizard
// prefills into the agent docker-compose template.
func (s *Server) handleDeployInfo(w http.ResponseWriter, r *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{
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
	req, ok := decodeJSON[AgentRegisterRequest](w, r, "invalid request")
	if !ok {
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
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "registration failed"})
		return
	}

	s.logger.Info("agent registered", "agent_id", req.ID, "hostname", req.Hostname, "address", req.Address)
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, s.registry.ListViews())
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent id"})
		return
	}

	if !s.registry.Delete(agentID) {
		addonutil.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}

	s.logger.Info("agent deleted", "agent_id", agentID)
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted", "agent_id": agentID})
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	// Accept both {"action":"status"} and {"command":"status"} since the
	// Vigil action proxy forwards form data as-is (which uses "command").
	type commandRequest struct {
		AgentID string          `json:"agent_id"`
		Action  string          `json:"action"`
		Command string          `json:"command"`
		Params  json.RawMessage `json:"params"`
	}
	raw, ok := decodeJSON[commandRequest](w, r, "invalid command")
	if !ok {
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

		addonutil.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	writeRawJSON(w, resp)
}

// TelemetryIngestRequest carries a raw telemetry frame from an Agent.
type TelemetryIngestRequest struct {
	AgentID string          `json:"agent_id"`
	Payload json.RawMessage `json:"payload"`
}

func (s *Server) handleTelemetryIngest(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSON[TelemetryIngestRequest](w, r, "invalid telemetry")
	if !ok {
		return
	}

	s.aggregator.IngestAgentFrame(req.AgentID, req.Payload)
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

// handleConfigFromBody handles POST /api/config where agent_id is in the JSON body.
// This is the path used by the Vigil action proxy (which sends to /api/{action}).
// The proxy sends flat form data: {"agent_id":"x", "key1":"val1", ...}
// The agent expects: {"values": {"key1":"val1", ...}}
func (s *Server) handleConfigFromBody(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}

	var flat map[string]interface{}
	if err := json.Unmarshal(body, &flat); err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	agentID, _ := flat["agent_id"].(string)
	if agentID == "" {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent_id in body"})
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
		addonutil.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "forwarded"})
}

func (s *Server) handleConfigForward(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	if agentID == "" {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent_id"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}

	if err := s.router.RouteConfigUpdate(agentID, body); err != nil {
		s.logger.Error("config forward failed", "agent_id", agentID, "error", err)
		addonutil.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "forwarded"})
}


// handleGetConfig proxies GET /api/config?agent_id=xxx to the target agent.
// If no agent_id is given, it returns config from the first online agent.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	agentID := s.resolveAgentID(r)
	if agentID == "" {
		addonutil.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "no online agents"})
		return
	}

	body, err := s.router.FetchAgentConfig(agentID)
	if err != nil {
		s.logger.Error("failed to fetch agent config", "agent_id", agentID, "error", err)
		addonutil.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	writeRawJSON(w, body)
}

func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	// The Vigil proxy sends form data as {"data": {"confirm": "ROTATE"}}
	type rotateRequest struct {
		Data struct {
			Confirm string `json:"confirm"`
		} `json:"data"`
		Confirm string `json:"confirm"` // direct call (not via proxy)
	}
	req, ok := decodeJSON[rotateRequest](w, r, "invalid request")
	if !ok {
		return
	}

	confirm := req.Data.Confirm
	if confirm == "" {
		confirm = req.Confirm
	}
	if confirm != "ROTATE" {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "type ROTATE to confirm"})
		return
	}

	newToken, err := GenerateToken()
	if err != nil {
		s.logger.Error("failed to generate new token", "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "token generation failed"})
		return
	}

	if err := PersistToken(s.cfg.Data.RegistryPath, newToken); err != nil {
		s.logger.Error("failed to persist new token", "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save token"})
		return
	}

	s.cfg.Vigil.Token = newToken
	s.logger.Info("hub token rotated successfully")
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{
		"status":    "rotated",
		"hub_token": newToken,
	})
}

// handleProxyToAgent forwards a GET request to the first online agent,
// preserving the request path and query string. Used for /api/jobs/history
// and /api/logs/history.
func (s *Server) handleProxyToAgent(w http.ResponseWriter, r *http.Request) {
	agentID := s.resolveAgentID(r)
	if agentID == "" {
		addonutil.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "no online agents"})
		return
	}

	// Build the path + query to forward.
	pathAndQuery := r.URL.Path
	if r.URL.RawQuery != "" {
		pathAndQuery += "?" + r.URL.RawQuery
	}

	body, statusCode, err := s.router.ProxyGet(agentID, pathAndQuery)
	if err != nil {
		s.logger.Error("proxy to agent failed", "agent_id", agentID, "error", err)
		addonutil.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	w.Write(body)
}

// telemetryFieldInfo maps a URL path to its cache key and agent fallback path.
type telemetryFieldInfo struct {
	telemetryKey string
	agentPath    string
}

var telemetryFieldMap = map[string]telemetryFieldInfo{
	"/api/disk_status":  {telemetryKey: "array_status", agentPath: "/api/array_status"},
	"/api/active_job":   {telemetryKey: "active_job", agentPath: "/api/active_job"},
	"/api/disk_storage": {telemetryKey: "disk_storage", agentPath: "/api/disk_storage"},
}

// handleTelemetryField serves cached telemetry data from the aggregator.
// When no cached data exists, falls back to fetching directly from the agent.
func (s *Server) handleTelemetryField(w http.ResponseWriter, r *http.Request) {
	info, ok := telemetryFieldMap[r.URL.Path]
	if !ok {
		addonutil.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "unknown telemetry field"})
		return
	}

	agentID := r.URL.Query().Get("agent_id")

	// Try cached telemetry first.
	if data := s.serveCachedTelemetry(w, agentID, info); data {
		return
	}

	// Fall back to fetching directly from agent.
	if agentID == "" {
		agentID = s.resolveAgentID(r)
	}
	if agentID != "" && s.proxyTelemetryFromAgent(w, agentID, info) {
		return
	}

	// Final fallback: empty response.
	writeRawJSON(w, telemetryEmptyResponse(info.telemetryKey))
}

// serveCachedTelemetry attempts to serve from the aggregator cache.
// Returns true if a response was written.
func (s *Server) serveCachedTelemetry(w http.ResponseWriter, agentID string, info telemetryFieldInfo) bool {
	data := s.aggregator.LatestTelemetryField(agentID, info.telemetryKey)
	if data == nil || string(data) == "null" {
		return false
	}

	// For array_status, extract the nested disk_status/disks array.
	if info.telemetryKey == "array_status" {
		if arr := extractDiskStatusArray(data); arr != nil {
			writeRawJSON(w, arr)
			return true
		}
		return false // fall through to agent proxy
	}

	writeRawJSON(w, data)
	return true
}

// extractDiskStatusArray pulls the "disk_status" or "disks" array from
// a nested array_status JSON object. Returns nil if not extractable.
func extractDiskStatusArray(data json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil
	}
	for _, key := range []string{"disk_status", "disks"} {
		if arr, exists := obj[key]; exists {
			return arr
		}
	}
	return nil
}

// proxyTelemetryFromAgent fetches the field directly from the agent.
// Returns true if a response was written.
func (s *Server) proxyTelemetryFromAgent(w http.ResponseWriter, agentID string, info telemetryFieldInfo) bool {
	s.logger.Debug("telemetry cache miss, proxying to agent", "agent_id", agentID, "path", info.agentPath)
	body, statusCode, err := s.router.ProxyGet(agentID, info.agentPath)
	if err != nil || statusCode >= 400 {
		s.logger.Warn("agent fallback failed", "agent_id", agentID, "path", info.agentPath, "status", statusCode, "error", err)
		return false
	}
	writeRawJSON(w, body)
	return true
}

// telemetryEmptyResponse returns the appropriate empty value for a telemetry field.
func telemetryEmptyResponse(key string) []byte {
	if key == "active_job" {
		return []byte("null")
	}
	return []byte("[]")
}

// writeRawJSON writes pre-encoded JSON bytes to the response.
func writeRawJSON(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleActiveJobs returns the current active job (if any) as a JSON array
// formatted as ProgressPayload objects for the progress component.
func (s *Server) handleActiveJobs(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	data := s.aggregator.LatestTelemetryField(agentID, "active_job")
	if data == nil || string(data) == "null" {
		addonutil.WriteJSON(w, http.StatusOK, []any{})
		return
	}

	// Parse the ActiveJob telemetry and transform it to the ProgressPayload
	// format expected by the frontend progress component.
	var activeJob struct {
		Type            string `json:"type"`
		StartedAt       string `json:"started_at"`
		ProgressPercent int    `json:"progress_percent"`
		CurrentPhase    string `json:"current_phase"`
	}
	if err := json.Unmarshal(data, &activeJob); err != nil {
		addonutil.WriteJSON(w, http.StatusOK, []any{})
		return
	}

	// Compute elapsed seconds from started_at.
	var elapsedSec int64
	if t, err := time.Parse(time.RFC3339Nano, activeJob.StartedAt); err == nil {
		elapsedSec = int64(time.Since(t).Seconds())
	}

	progressPayload := map[string]any{
		"job_id":      activeJob.Type + "_" + activeJob.StartedAt,
		"command":     activeJob.Type,
		"phase":       activeJob.CurrentPhase,
		"percent":     activeJob.ProgressPercent,
		"elapsed_sec": elapsedSec,
	}

	addonutil.WriteJSON(w, http.StatusOK, []any{progressPayload})
}

// handleCancelJob aborts the active job on the agent. The job ID path param
// is accepted for API compatibility with the progress component's cancel
// button, but SnapRaid only has one active job at a time so it simply sends
// an abort command to the appropriate agent.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	agentID := s.resolveAgentID(r)
	if agentID == "" {
		addonutil.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "no online agent found"})
		return
	}

	_, err := s.router.RouteCommand(CommandMessage{
		AgentID: agentID,
		Action:  "abort",
	})
	if err != nil {
		s.logger.Error("cancel job failed", "agent_id", agentID, "error", err)
		addonutil.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

