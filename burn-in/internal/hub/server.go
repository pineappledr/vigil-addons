package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// Server is the HTTP server for the burn-in hub.
type Server struct {
	registry   *AgentRegistry
	router     *CommandRouter
	aggregator *Aggregator
	psk        string
	logger     *slog.Logger
	mux        *http.ServeMux
}

// NewServer creates the hub HTTP server with all routes registered.
func NewServer(registry *AgentRegistry, aggregator *Aggregator, psk string, logger *slog.Logger) *Server {
	s := &Server{
		registry:   registry,
		router:     NewCommandRouter(registry, logger),
		aggregator: aggregator,
		psk:        psk,
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
	s.mux.HandleFunc("POST /api/agents/register", s.requirePSK(s.handleRegisterAgent))
	s.mux.HandleFunc("POST /api/execute", s.router.HandleExecute)
	s.mux.HandleFunc("GET /api/agents/{id}/telemetry", s.aggregator.HandleAgentTelemetry)
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

func (s *Server) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	agents := s.registry.List()
	writeJSON(w, http.StatusOK, agents)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
