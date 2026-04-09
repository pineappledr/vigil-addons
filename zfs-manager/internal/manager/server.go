package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
	"github.com/pineappledr/vigil-addons/zfs-manager/internal/config"
)

// Server is the manager HTTP server.
type Server struct {
	cfg        *config.ManagerConfig
	registry   *Registry
	aggregator *Aggregator
	mux        *http.ServeMux
	logger     *slog.Logger
	pskMu      sync.RWMutex
	psk        string
}

// NewServer creates the manager HTTP server.
func NewServer(cfg *config.ManagerConfig, registry *Registry, aggregator *Aggregator, psk string, logger *slog.Logger) *Server {
	s := &Server{
		cfg:        cfg,
		registry:   registry,
		aggregator: aggregator,
		mux:        http.NewServeMux(),
		logger:     logger,
		psk:        psk,
	}
	s.routes()
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) getPSK() string {
	s.pskMu.RLock()
	defer s.pskMu.RUnlock()
	return s.psk
}

func (s *Server) requirePSK(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			addonutil.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing authorization header"})
			return
		}
		if strings.TrimPrefix(auth, "Bearer ") != s.getPSK() {
			addonutil.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "invalid pre-shared key"})
			return
		}
		next(w, r)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/deploy-info", s.handleDeployInfo)
	s.mux.HandleFunc("POST /api/agents/register", s.requirePSK(s.handleAgentRegister))
	s.mux.HandleFunc("GET /api/agents", s.handleAgentList)
	s.mux.HandleFunc("DELETE /api/agents/{id}", s.handleAgentDelete)
	s.mux.HandleFunc("POST /api/telemetry/ingest", s.requirePSK(s.handleTelemetryIngest))
	s.mux.HandleFunc("GET /api/telemetry/{agentID}", s.handleTelemetryGet)
	s.mux.HandleFunc("GET /api/pools", s.handlePools)
	s.mux.HandleFunc("GET /api/datasets", s.handleDatasets)
	s.mux.HandleFunc("GET /api/snapshots", s.handleSnapshots)
	s.mux.HandleFunc("GET /api/presets", s.handlePresets)
	s.mux.HandleFunc("POST /api/rotate-psk", s.handleRotatePSK)

	// Phase 2 — command proxy (routes to agent)
	s.mux.HandleFunc("POST /api/datasets", s.proxyToAgent)
	s.mux.HandleFunc("PUT /api/datasets", s.proxyToAgent)
	s.mux.HandleFunc("DELETE /api/datasets", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/snapshots", s.proxyToAgent)
	s.mux.HandleFunc("DELETE /api/snapshots", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/snapshots/rollback", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/scrub/start", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/scrub/pause", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/scrub/cancel", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/preview", s.proxyToAgent)

	// Phase 4 — disk & pool operations proxy (routes to agent)
	s.mux.HandleFunc("GET /api/disks", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/pool/replace", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/pool/add-vdev", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/devices/offline", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/devices/online", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/pool/clear", s.proxyToAgent)

	// Phase 3 — scheduled tasks proxy (routes to agent)
	s.mux.HandleFunc("GET /api/tasks", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/tasks", s.proxyToAgent)
	s.mux.HandleFunc("PUT /api/tasks/{id}", s.proxyToAgent)
	s.mux.HandleFunc("DELETE /api/tasks/{id}", s.proxyToAgent)
	s.mux.HandleFunc("GET /api/tasks/{id}/history", s.proxyToAgent)
	s.mux.HandleFunc("GET /api/jobs", s.proxyToAgent)
	s.mux.HandleFunc("GET /api/retention", s.proxyToAgent)
	s.mux.HandleFunc("POST /api/retention/cleanup", s.proxyToAgent)
}

// resolveAgentID returns the agent_id from the query string, falling back to
// the first online agent in the registry.
func (s *Server) resolveAgentID(r *http.Request) string {
	if id := r.URL.Query().Get("agent_id"); id != "" {
		return id
	}
	for _, v := range s.registry.ListViews() {
		if v.Status == "online" {
			return v.AgentEntry.ID
		}
	}
	return ""
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDeployInfo(w http.ResponseWriter, r *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{
		"hub_url": fmt.Sprintf("http://%s:%d", r.Host, s.cfg.Listen.Port),
		"hub_psk": s.getPSK(),
	})
}

type agentRegisterRequest struct {
	AgentID       string `json:"agent_id"`
	Hostname      string `json:"hostname"`
	Arch          string `json:"arch"`
	AdvertiseAddr string `json:"advertise_addr"`
	Version       string `json:"version"`
}

func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var req agentRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if req.AgentID == "" {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id required"})
		return
	}

	entry := AgentEntry{
		ID:       req.AgentID,
		Hostname: req.Hostname,
		Arch:     req.Arch,
		Address:  req.AdvertiseAddr,
		Version:  req.Version,
	}
	if err := s.registry.Register(entry); err != nil {
		s.logger.Error("failed to register agent", "agent_id", req.AgentID, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "registration failed"})
		return
	}

	s.logger.Info("agent registered", "agent_id", req.AgentID, "hostname", req.Hostname)
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

func (s *Server) handleAgentList(w http.ResponseWriter, _ *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, s.registry.ListViews())
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.registry.Delete(id) {
		addonutil.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type telemetryIngestRequest struct {
	AgentID string          `json:"agent_id"`
	Payload json.RawMessage `json:"payload"`
}

func (s *Server) handleTelemetryIngest(w http.ResponseWriter, r *http.Request) {
	var req telemetryIngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid telemetry"})
		return
	}
	s.registry.Touch(req.AgentID)
	s.aggregator.Ingest(req.AgentID, req.Payload)
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

func (s *Server) handleTelemetryGet(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	data := s.aggregator.Latest(agentID)
	if data == nil {
		addonutil.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "no telemetry for agent"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// serveAgentField extracts a named field from the aggregator cache for the
// resolved agent and writes it as JSON. Falls back to an empty JSON array.
func (s *Server) serveAgentField(w http.ResponseWriter, r *http.Request, field string) {
	agentID := s.resolveAgentID(r)
	if agentID != "" {
		if data := s.aggregator.LatestField(agentID, field); data != nil && string(data) != "null" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
	}
	// No cached data — return empty array so the table renders cleanly.
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("[]"))
}

func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	s.serveAgentField(w, r, "pools")
}

func (s *Server) handleDatasets(w http.ResponseWriter, r *http.Request) {
	s.serveAgentField(w, r, "datasets")
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	s.serveAgentField(w, r, "snapshots")
}

func (s *Server) handlePresets(w http.ResponseWriter, _ *http.Request) {
	// Return dataset presets for the UI wizard.
	presets := map[string]map[string]string{
		"general": {"name": "General Purpose", "record_size": "128K", "compression": "lz4", "atime": "off", "sync": "standard"},
		"media":   {"name": "Media Storage", "record_size": "1M", "compression": "lz4", "atime": "off", "sync": "disabled"},
		"vm":      {"name": "VM/App Storage", "record_size": "64K", "compression": "lz4", "atime": "off", "sync": "standard"},
		"db":      {"name": "Database", "record_size": "16K", "compression": "lz4", "atime": "off", "sync": "always"},
	}
	addonutil.WriteJSON(w, http.StatusOK, presets)
}

// proxyToAgent forwards a request to the resolved agent's HTTP API.
// The agent is selected via the ?agent_id= query parameter (or first online agent).
func (s *Server) proxyToAgent(w http.ResponseWriter, r *http.Request) {
	agentID := s.resolveAgentID(r)
	if agentID == "" {
		addonutil.WriteError(w, http.StatusBadGateway, "no agent available")
		return
	}

	entry := s.registry.Get(agentID)
	if entry == nil {
		addonutil.WriteError(w, http.StatusNotFound, "agent not found: "+agentID)
		return
	}
	if entry.Address == "" {
		addonutil.WriteError(w, http.StatusBadGateway, "agent has no advertise address")
		return
	}

	// Build the upstream URL: parse the agent's advertised address and enforce
	// an http/https scheme allowlist before appending the original path. The
	// address is set by PSK-authenticated agents via handleAgentRegister, but
	// we still validate the scheme as defense in depth against SSRF gadgets.
	base, err := url.Parse(strings.TrimRight(entry.Address, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		addonutil.WriteError(w, http.StatusBadGateway, "agent has invalid advertise address")
		return
	}
	base.Path = path.Join(base.Path, r.URL.Path)
	targetURL := base.String()

	// Read the original body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// #nosec G107 -- targetURL is built from a PSK-authenticated agent registry
	// entry whose scheme is restricted to http/https above.
	proxyReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, "failed to create proxy request")
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")

	// #nosec G107 -- see targetURL construction above.
	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		s.logger.Error("agent proxy failed", "agent_id", agentID, "url", targetURL, "error", err)
		addonutil.WriteError(w, http.StatusBadGateway, "agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Forward the agent's response back to the caller
	respBody, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func (s *Server) handleRotatePSK(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Confirm != "ROTATE" {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "type ROTATE to confirm"})
		return
	}

	newPSK, err := generateRandom()
	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "PSK generation failed"})
		return
	}
	if err := PersistPSK(s.cfg.Data.RegistryPath, newPSK); err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save PSK"})
		return
	}

	s.pskMu.Lock()
	s.psk = newPSK
	s.pskMu.Unlock()

	s.logger.Info("PSK rotated")
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "rotated", "hub_psk": newPSK})
}
