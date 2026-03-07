package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

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
	s.mux.HandleFunc("GET /api/config", s.handleGetConfig)
	s.mux.HandleFunc("POST /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/jobs", s.handleJobs)
	s.mux.HandleFunc("GET /api/jobs/history", s.handleJobHistory)
	s.mux.HandleFunc("GET /api/logs/history", s.handleLogHistory)
	s.mux.HandleFunc("GET /api/array_status", s.handleArrayStatus)
	s.mux.HandleFunc("GET /api/smart_status", s.handleSmartStatus)
	s.mux.HandleFunc("GET /api/active_job", s.handleActiveJob)
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

	// Record the job in history so it appears in the Logs tab.
	jobID, _ := agentdb.InsertJob(s.db, req.Command, "manual")

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
			output = report.Output
		}
	case "smart":
		var report *engine.SmartReport
		report, execErr = s.engine.Smart(ctx)
		if report != nil {
			s.collector.SetSmartStatus(report)
			output = report.Output
		}
	case "diff":
		var report *engine.DiffReport
		report, execErr = s.engine.Diff(ctx)
		if report != nil {
			s.collector.SetDiffStatus(report)
			output = report.Output
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
		if jobID > 0 {
			agentdb.CompleteJob(s.db, jobID, -1, "error", execErr.Error())
		}
		status := http.StatusInternalServerError
		if execErr == engine.ErrEngineLocked {
			status = http.StatusConflict
		}
		writeJSON(w, status, ExecuteResponse{Status: "error", Error: execErr.Error()})
		return
	}

	if jobID > 0 {
		agentdb.CompleteJob(s.db, jobID, exitCode, "success", output)
	}

	// Log the full command output to container logs.
	if output != "" {
		s.logger.Info("command output", "command", req.Command, "output", output)
	}

	writeJSON(w, http.StatusOK, ExecuteResponse{
		Status:   "ok",
		ExitCode: exitCode,
		Output:   output,
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	values, err := agentdb.GetAllCacheValues(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, values)
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

// handleJobHistory returns job records with optional time_range filtering.
// Query params: time_range (e.g. "24h", "7d", "30d")
func (s *Server) handleJobHistory(w http.ResponseWriter, r *http.Request) {
	var jobs []agentdb.JobRecord
	var err error

	if tr := r.URL.Query().Get("time_range"); tr != "" {
		if d, ok := parseTimeRange(tr); ok {
			since := time.Now().UTC().Add(-d)
			jobs, err = agentdb.RecentJobsSince(s.db, since, 200)
		} else {
			jobs, err = agentdb.RecentJobs(s.db, 200)
		}
	} else {
		jobs, err = agentdb.RecentJobs(s.db, 200)
	}

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if jobs == nil {
		jobs = []agentdb.JobRecord{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// LogEntry is a log line for the log-viewer component.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Source    string `json:"source"`
	Message   string `json:"message"`
}

// handleLogHistory returns job output logs formatted for the log-viewer.
func (s *Server) handleLogHistory(w http.ResponseWriter, r *http.Request) {
	var jobs []agentdb.JobRecord
	var err error

	if tr := r.URL.Query().Get("time_range"); tr != "" {
		if d, ok := parseTimeRange(tr); ok {
			since := time.Now().UTC().Add(-d)
			jobs, err = agentdb.RecentJobsSince(s.db, since, 100)
		} else {
			jobs, err = agentdb.RecentJobs(s.db, 100)
		}
	} else {
		jobs, err = agentdb.RecentJobs(s.db, 100)
	}

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var entries []LogEntry
	for _, j := range jobs {
		level := "info"
		if j.Status == "error" || j.Status == "failed" {
			level = "error"
		} else if j.Status == "running" {
			level = "warn"
		}

		msg := fmt.Sprintf("%s — %s", j.Trigger, j.Status)
		if j.ExitCode != nil {
			msg += fmt.Sprintf(" (exit %d)", *j.ExitCode)
		}

		entries = append(entries, LogEntry{
			Timestamp: j.StartedAt.UTC().Format(time.RFC3339),
			Level:     level,
			Source:    j.JobType,
			Message:   msg,
		})

		// If job has output, add it as a separate log line
		if j.OutputLog != "" {
			// Truncate very long output for the log viewer
			output := j.OutputLog
			if len(output) > 8000 {
				output = output[:8000] + "... (truncated)"
			}
			entries = append(entries, LogEntry{
				Timestamp: j.StartedAt.UTC().Format(time.RFC3339),
				Level:     "info",
				Source:    j.JobType,
				Message:   output,
			})
		}
	}

	if entries == nil {
		entries = []LogEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleArrayStatus returns the cached disk status from the collector.
// If no cache exists, runs `snapraid status` on demand to populate it.
func (s *Server) handleArrayStatus(w http.ResponseWriter, r *http.Request) {
	report := s.collector.GetArrayStatus()
	if report == nil {
		// Run status on-demand to populate the cache.
		fresh, err := s.engine.Status(r.Context())
		if err != nil {
			writeJSON(w, http.StatusOK, []any{}) // empty array, not error
			return
		}
		s.collector.SetArrayStatus(fresh)
		report = s.collector.GetArrayStatus()
	}
	if report == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, report.DiskStatus)
}

// handleSmartStatus returns the cached SMART disk data from the collector.
// If no cache exists, runs `snapraid smart` on demand to populate it.
func (s *Server) handleSmartStatus(w http.ResponseWriter, r *http.Request) {
	report := s.collector.GetSmartStatus()
	if report == nil {
		fresh, err := s.engine.Smart(r.Context())
		if err != nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		s.collector.SetSmartStatus(fresh)
		report = s.collector.GetSmartStatus()
	}
	if report == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, report.Disks)
}

// handleActiveJob returns the current active job, or null if none.
func (s *Server) handleActiveJob(w http.ResponseWriter, r *http.Request) {
	job := s.collector.GetActiveJob()
	if job == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("null"))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// parseTimeRange converts strings like "24h", "7d", "30d", "5m" to a duration.
func parseTimeRange(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return 0, false
	}
	switch unit {
	case 'm':
		return time.Duration(n) * time.Minute, true
	case 'h':
		return time.Duration(n) * time.Hour, true
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	default:
		return 0, false
	}
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
