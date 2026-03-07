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
	s.mux.HandleFunc("DELETE /api/agents/{id}", s.handleAgentDelete)
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
	var cmd CommandMessage
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid command"})
		return
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
func (s *Server) handleConfigFromBody(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}

	var envelope struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.AgentID == "" {
		writeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent_id in body"})
		return
	}

	if err := s.router.RouteConfigUpdate(envelope.AgentID, body); err != nil {
		s.logger.Error("config forward failed", "agent_id", envelope.AgentID, "error", err)
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

func writeHubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
