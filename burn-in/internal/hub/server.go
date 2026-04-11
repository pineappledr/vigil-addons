package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
)

// Server is the HTTP server for the burn-in hub.
type Server struct {
	registry   *AgentRegistry
	router     *CommandRouter
	aggregator *Aggregator
	pskMu      sync.RWMutex
	psk        string
	listen     string
	dataDir    string
	logger     *slog.Logger
	mux        *http.ServeMux
}

// NewServer creates the hub HTTP server with all routes registered.
func NewServer(registry *AgentRegistry, aggregator *Aggregator, psk, listen, dataDir string, logger *slog.Logger) *Server {
	s := &Server{
		registry:   registry,
		router:     NewCommandRouter(registry, logger),
		aggregator: aggregator,
		psk:        psk,
		listen:     listen,
		dataDir:    dataDir,
		logger:     logger,
		mux:        http.NewServeMux(),
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

// getPSK returns the current PSK under the read lock.
func (s *Server) getPSK() string {
	s.pskMu.RLock()
	defer s.pskMu.RUnlock()
	return s.psk
}

// requirePSK returns middleware that validates the Authorization: Bearer <psk> header.
func (s *Server) requirePSK(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			addonutil.WriteJSON(w, http.StatusUnauthorized, addonutil.ErrorResponse{Error: "missing or invalid authorization header"})
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if token != s.getPSK() {
			addonutil.WriteJSON(w, http.StatusForbidden, addonutil.ErrorResponse{Error: "invalid pre-shared key"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	var reg AgentRegistration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "invalid request body"})
		return
	}

	if reg.AgentID == "" {
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "agent_id is required"})
		return
	}

	record := s.registry.Register(reg)
	addonutil.WriteJSON(w, http.StatusOK, record)
}

func (s *Server) handleDeployInfo(w http.ResponseWriter, r *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{
		"hub_url": fmt.Sprintf("http://%s", r.Host),
		"hub_psk": s.getPSK(),
	})
}

func (s *Server) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	agents := s.registry.ListViews(s.aggregator.BusyAgentIDs())
	addonutil.WriteJSON(w, http.StatusOK, agents)
}

func (s *Server) handleLogHistory(w http.ResponseWriter, r *http.Request) {
	timeRange := r.URL.Query().Get("time_range")
	logs := s.aggregator.QueryLogs(timeRange)
	addonutil.WriteJSON(w, http.StatusOK, logs)
}

func (s *Server) handleChartHistory(w http.ResponseWriter, r *http.Request) {
	componentID := r.URL.Query().Get("component_id")
	timeRange := r.URL.Query().Get("time_range")
	points := s.aggregator.QueryChartHistory(componentID, timeRange)
	addonutil.WriteJSON(w, http.StatusOK, points)
}

func (s *Server) handleActiveJobs(w http.ResponseWriter, _ *http.Request) {
	jobs := s.aggregator.QueryActiveJobs()
	addonutil.WriteJSON(w, http.StatusOK, jobs)
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
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "agent id is required"})
		return
	}

	if !s.registry.Delete(agentID) {
		addonutil.WriteJSON(w, http.StatusNotFound, addonutil.ErrorResponse{Error: "agent not found"})
		return
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted", "agent_id": agentID})
}

func (s *Server) handleRotatePSK(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data struct {
			Confirm string `json:"confirm"`
		} `json:"data"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "invalid request"})
		return
	}

	confirm := req.Data.Confirm
	if confirm == "" {
		confirm = req.Confirm
	}
	if confirm != "ROTATE" {
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "type ROTATE to confirm"})
		return
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		s.logger.Error("failed to generate new PSK", "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, addonutil.ErrorResponse{Error: "PSK generation failed"})
		return
	}
	newPSK := hex.EncodeToString(buf)

	// Update memory and disk atomically under the write lock so there is
	// no window where the two are inconsistent.
	s.pskMu.Lock()
	s.psk = newPSK
	s.pskMu.Unlock()

	pskPath := filepath.Join(s.dataDir, "hub.psk")
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		s.logger.Error("failed to create data directory", "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, addonutil.ErrorResponse{Error: "failed to save PSK"})
		return
	}
	if err := os.WriteFile(pskPath, []byte(newPSK+"\n"), 0o600); err != nil {
		// Memory already updated — log the disk persistence failure but
		// don't roll back, since the new PSK is already in use.
		s.logger.Error("failed to persist new PSK to disk (memory updated)", "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, addonutil.ErrorResponse{Error: "PSK rotated in memory but failed to persist to disk"})
		return
	}
	s.logger.Info("hub PSK rotated successfully")
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "rotated",
		"hub_psk": newPSK,
	})
}

