package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Server is the HTTP server for the burn-in hub.
type Server struct {
	registry     *AgentRegistry
	router       *CommandRouter
	aggregator   *Aggregator
	psk          string
	advertiseURL string
	dataDir      string
	logger       *slog.Logger
	mux          *http.ServeMux
}

// NewServer creates the hub HTTP server with all routes registered.
func NewServer(registry *AgentRegistry, aggregator *Aggregator, psk, advertiseURL, dataDir string, logger *slog.Logger) *Server {
	s := &Server{
		registry:     registry,
		router:       NewCommandRouter(registry, logger),
		aggregator:   aggregator,
		psk:          psk,
		advertiseURL: advertiseURL,
		dataDir:      dataDir,
		logger:       logger,
		mux:          http.NewServeMux(),
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
	s.mux.HandleFunc("GET /api/agents", s.handleListAgents)
	s.mux.HandleFunc("GET /api/deploy-info", s.handleDeployInfo)
	s.mux.HandleFunc("POST /api/agents/register", s.requirePSK(s.handleRegisterAgent))
	s.mux.HandleFunc("DELETE /api/agents/{id}", s.handleDeleteAgent)
	s.mux.HandleFunc("POST /api/execute", s.router.HandleExecute)
	s.mux.HandleFunc("GET /api/jobs/history", s.router.HandleJobHistory)
	s.mux.HandleFunc("DELETE /api/jobs/{id}", s.router.HandleCancelJob)
	s.mux.HandleFunc("GET /api/logs/history", s.handleLogHistory)
	s.mux.HandleFunc("GET /api/chart/history", s.handleChartHistory)
	s.mux.HandleFunc("GET /api/jobs/active", s.handleActiveJobs)
	s.mux.HandleFunc("GET /api/smart/deltas", s.handleSmartDeltas)
	s.mux.HandleFunc("GET /api/agents/{id}/telemetry", s.aggregator.HandleAgentTelemetry)
	s.mux.HandleFunc("POST /api/rotate-psk", s.handleRotatePSK)
}

// requirePSK returns middleware that validates the Authorization: Bearer <psk> header.
func (s *Server) requirePSK(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "missing or invalid authorization header"})
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if token != s.psk {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "invalid pre-shared key"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	var reg AgentRegistration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	if reg.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "agent_id is required"})
		return
	}

	record := s.registry.Register(reg)
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) handleDeployInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"hub_url": s.advertiseURL,
		"hub_psk": s.psk,
	})
}

func (s *Server) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	agents := s.registry.ListViews(s.aggregator.BusyAgentIDs())
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) handleLogHistory(w http.ResponseWriter, r *http.Request) {
	timeRange := r.URL.Query().Get("time_range")
	logs := s.aggregator.QueryLogs(timeRange)
	writeJSON(w, http.StatusOK, logs)
}

func (s *Server) handleChartHistory(w http.ResponseWriter, r *http.Request) {
	componentID := r.URL.Query().Get("component_id")
	timeRange := r.URL.Query().Get("time_range")
	points := s.aggregator.QueryChartHistory(componentID, timeRange)
	writeJSON(w, http.StatusOK, points)
}

func (s *Server) handleActiveJobs(w http.ResponseWriter, _ *http.Request) {
	jobs := s.aggregator.QueryActiveJobs()
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleSmartDeltas(w http.ResponseWriter, r *http.Request) {
	deltas := s.aggregator.QuerySmartDeltas()
	if deltas == nil {
		// Return empty object so the client sees valid JSON.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(deltas)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "agent id is required"})
		return
	}

	if !s.registry.Delete(agentID) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "agent not found"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "agent_id": agentID})
}

func (s *Server) handleRotatePSK(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data struct {
			Confirm string `json:"confirm"`
		} `json:"data"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request"})
		return
	}

	confirm := req.Data.Confirm
	if confirm == "" {
		confirm = req.Confirm
	}
	if confirm != "ROTATE" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "type ROTATE to confirm"})
		return
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		s.logger.Error("failed to generate new PSK", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "PSK generation failed"})
		return
	}
	newPSK := hex.EncodeToString(buf)

	pskPath := filepath.Join(s.dataDir, "hub.psk")
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		s.logger.Error("failed to create data directory", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to save PSK"})
		return
	}
	if err := os.WriteFile(pskPath, []byte(newPSK+"\n"), 0o600); err != nil {
		s.logger.Error("failed to persist new PSK", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to save PSK"})
		return
	}

	s.psk = newPSK
	s.logger.Info("hub PSK rotated successfully")
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "rotated",
		"hub_psk": newPSK,
	})
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
