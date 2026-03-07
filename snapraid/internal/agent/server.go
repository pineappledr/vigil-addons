package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	agentdb "github.com/pineappledr/vigil-addons/snapraid/internal/db"
	"github.com/pineappledr/vigil-addons/snapraid/internal/engine"
)

// Server is the Agent HTTP server exposing health, execute, and config endpoints.
type Server struct {
	cfg       *config.AgentConfig
	engine    *engine.Engine
	db        *sql.DB
	collector *Collector
	mux       *http.ServeMux
	server    *http.Server
	logger    *slog.Logger
}

// NewServer creates the Agent server with all dependencies.
func NewServer(cfg *config.AgentConfig, eng *engine.Engine, database *sql.DB, collector *Collector, logger *slog.Logger) *Server {
	s := &Server{
		cfg:       cfg,
		engine:    eng,
		db:        database,
		collector: collector,
		mux:       http.NewServeMux(),
		logger:    logger,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /api/execute", s.handleExecute)
	s.mux.HandleFunc("POST /api/abort", s.handleAbort)
	s.mux.HandleFunc("POST /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/jobs", s.handleJobs)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"status":"ok"}`)
}

// ExecuteRequest is the payload for POST /api/execute.
type ExecuteRequest struct {
	Command       string `json:"command"`
	BadBlocksOnly bool   `json:"bad_blocks_only,omitempty"`
	Disk          string `json:"disk,omitempty"`
	Filter        string `json:"filter,omitempty"`
	Plan          string `json:"plan,omitempty"`
	OlderThanDays int    `json:"older_than_days,omitempty"`
	ForceZero     bool   `json:"force_zero,omitempty"`
	PreHash       bool   `json:"pre_hash,omitempty"`
}

// ExecuteResponse is the response from POST /api/execute.
type ExecuteResponse struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ExecuteResponse{Status: "error", Error: "invalid request body"})
		return
	}

	ctx := r.Context()
	s.logger.Info("execute requested", "command", req.Command)

	var exitCode int
	var output string
	var execErr error

	switch req.Command {
	case "sync":
		var report *engine.SyncReport
		report, execErr = s.engine.Sync(ctx, engine.SyncOptions{
			PreHash:   req.PreHash,
			ForceZero: req.ForceZero,
		}, nil)
		if report != nil {
			exitCode = report.ExitCode
			output = report.Output
		}
	case "scrub":
		plan := req.Plan
		if plan == "" {
			plan = s.cfg.Scrub.Plan
		}
		days := req.OlderThanDays
		if days == 0 {
			days = s.cfg.Scrub.OlderThanDays
		}
		var report *engine.ScrubReport
		report, execErr = s.engine.Scrub(ctx, engine.ScrubOptions{Plan: plan, OlderThanDays: days})
		if report != nil {
			exitCode = report.ExitCode
			output = report.Output
		}
	case "fix":
		var report *engine.FixReport
		report, execErr = s.engine.Fix(ctx, engine.FixOptions{
			BadBlocksOnly: req.BadBlocksOnly,
			Disk:          req.Disk,
			Filter:        req.Filter,
		})
		if report != nil {
			exitCode = report.ExitCode
			output = report.Output
		}
	case "status":
		var report *engine.StatusReport
		report, execErr = s.engine.Status(ctx)
		if report != nil {
			s.collector.SetArrayStatus(report)
		}
	case "smart":
		var report *engine.SmartReport
		report, execErr = s.engine.Smart(ctx)
		if report != nil {
			s.collector.SetSmartStatus(report)
		}
	case "diff":
		var report *engine.DiffReport
		report, execErr = s.engine.Diff(ctx)
		if report != nil {
			s.collector.SetDiffStatus(report)
		}
	case "touch":
		var report *engine.TouchReport
		report, execErr = s.engine.Touch(ctx)
		if report != nil {
			exitCode = report.ExitCode
			output = report.Output
		}
	default:
		writeJSON(w, http.StatusBadRequest, ExecuteResponse{Status: "error", Error: "unknown command: " + req.Command})
		return
	}

	if execErr != nil {
		status := http.StatusInternalServerError
		if execErr == engine.ErrEngineLocked {
			status = http.StatusConflict
		}
		writeJSON(w, status, ExecuteResponse{Status: "error", Error: execErr.Error()})
		return
	}

	writeJSON(w, http.StatusOK, ExecuteResponse{
		Status:   "ok",
		ExitCode: exitCode,
		Output:   output,
	})
}

// ConfigUpdateRequest is the payload for POST /api/config.
type ConfigUpdateRequest struct {
	Values map[string]string `json:"values"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var req ConfigUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	for key, value := range req.Values {
		s.logger.Info("config value received", "key", key, "value", value)
		if err := agentdb.SetCacheValue(s.db, key, value); err != nil {
			s.logger.Error("failed to persist config value", "key", key, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist config"})
			return
		}
	}

	s.logger.Info("config updated", "keys_count", len(req.Values))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Abort(); err != nil {
		status := http.StatusConflict
		if err == engine.ErrNoActiveJob {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	s.logger.Info("active operation aborted via API")
	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := agentdb.RecentJobs(s.db, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.cfg.Listen.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.mux,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("agent listen on %s: %w", addr, err)
	}

	s.logger.Info("agent server started", "addr", addr)
	return s.server.Serve(ln)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("agent server shutting down")
	return s.server.Shutdown(ctx)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
