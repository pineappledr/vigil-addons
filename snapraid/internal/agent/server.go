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
	"sync/atomic"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	agentdb "github.com/pineappledr/vigil-addons/snapraid/internal/db"
	"github.com/pineappledr/vigil-addons/snapraid/internal/engine"
	"github.com/pineappledr/vigil-addons/snapraid/internal/scheduler"
)

// Server is the Agent HTTP server exposing health, execute, and config endpoints.
type Server struct {
	cfg       *config.AgentConfig
	engine    *engine.Engine
	db        *sql.DB
	collector *Collector
	sched     *scheduler.Scheduler
	schedCtx  context.Context
	mux       *http.ServeMux
	server    *http.Server
	logger    *slog.Logger

	refreshingStatus atomic.Bool
	refreshingSmart  atomic.Bool
}

// NewServer creates the Agent server with all dependencies.
func NewServer(cfg *config.AgentConfig, eng *engine.Engine, database *sql.DB, collector *Collector, sched *scheduler.Scheduler, schedCtx context.Context, logger *slog.Logger) *Server {
	s := &Server{
		cfg:      cfg,
		engine:   eng,
		db:       database,
		collector: collector,
		sched:    sched,
		schedCtx: schedCtx,
		mux:      http.NewServeMux(),
		logger:   logger,
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
	s.mux.HandleFunc("GET /api/jobs/{id}", s.handleJobDetail)
	s.mux.HandleFunc("GET /api/logs/history", s.handleLogHistory)
	s.mux.HandleFunc("GET /api/array_status", s.handleArrayStatus)
	s.mux.HandleFunc("GET /api/smart_status", s.handleSmartStatus)
	s.mux.HandleFunc("GET /api/active_job", s.handleActiveJob)
	s.mux.HandleFunc("GET /api/disk_storage", s.handleDiskStorage)
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
		addonutil.WriteJSON(w, http.StatusBadRequest, ExecuteResponse{Status: "error", Error: "invalid request body"})
		return
	}

	s.logger.Info("execute requested", "command", req.Command)

	// Validate command before accepting.
	switch req.Command {
	case "sync", "scrub", "fix", "status", "smart", "diff", "touch":
		// valid
	default:
		addonutil.WriteJSON(w, http.StatusBadRequest, ExecuteResponse{Status: "error", Error: "unknown command: " + req.Command})
		return
	}

	// Check if the engine is already busy (fail fast without blocking).
	if s.engine.IsBusy() {
		addonutil.WriteJSON(w, http.StatusConflict, ExecuteResponse{Status: "error", Error: engine.ErrEngineLocked.Error()})
		return
	}

	// Return immediately — the command runs in the background.
	// The Active Job tracker and job history record progress.
	addonutil.WriteJSON(w, http.StatusOK, ExecuteResponse{Status: "accepted"})

	go s.runCommand(req)
}

// progressChan creates a buffered progress channel and starts a goroutine
// that drains it into the collector's active job. Returns the channel and a
// stop function that must be called when the command finishes.
func (s *Server) progressChan() (chan<- int, func()) {
	ch := make(chan int, 4)
	done := make(chan struct{})
	go func() {
		for pct := range ch {
			s.collector.UpdateProgress(pct)
		}
		close(done)
	}()
	return ch, func() { close(ch); <-done }
}

// runCommand executes a snapraid command in the background with job tracking.
func (s *Server) runCommand(req ExecuteRequest) {
	ctx := context.Background()

	// Record the job in history so it appears in the Logs tab.
	jobID, _ := agentdb.InsertJob(s.db, req.Command, "manual")

	// Track the active job so the dashboard shows it while running.
	s.collector.TrackJob(req.Command, "manual", "running")
	defer s.collector.ClearJob()

	// Emit a start log line for real-time display.
	s.collector.EmitLogLine(req.Command, "info", req.Command+" started (manual)")

	progress, stopProgress := s.progressChan()
	defer stopProgress()

	var exitCode int
	var output string
	var execErr error

	switch req.Command {
	case "sync":
		var report *engine.SyncReport
		report, execErr = s.engine.Sync(ctx, engine.SyncOptions{
			PreHash:   req.PreHash,
			ForceZero: req.ForceZero,
		}, progress)
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
		report, execErr = s.engine.Scrub(ctx, engine.ScrubOptions{Plan: plan, OlderThanDays: days}, progress)
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
		}, progress)
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
	}

	if execErr != nil {
		if jobID > 0 {
			agentdb.CompleteJob(s.db, jobID, -1, "error", execErr.Error())
		}
		s.collector.EmitLogLine(req.Command, "error", req.Command+" failed: "+execErr.Error())
		s.logger.Error("command failed", "command", req.Command, "error", execErr)
		msg := "🔴 SnapRAID " + req.Command + " (manual) failed: " + execErr.Error()
		s.collector.EmitEvent("job_failed", "critical", msg)
		return
	}

	// Emit the command output as a real-time log line.
	if output != "" {
		s.collector.EmitLogLine(req.Command, "info", output)
	}

	status := "success"
	if exitCode == 2 {
		status = "warning"
	} else if exitCode != 0 {
		status = "error"
	}

	if jobID > 0 {
		agentdb.CompleteJob(s.db, jobID, exitCode, status, output)
	}

	// Log the full command output to container logs.
	if output != "" {
		s.logger.Info("command output", "command", req.Command, "output", output)
	}

	// Emit a completion notification with a parsed summary.
	msg := "✅ SnapRAID " + req.Command + " (manual) completed"
	if summary := scheduler.FormatCommandSummary(req.Command, output); summary != "" {
		msg += "\n\n" + summary
	}
	// For sync and scrub, append a status overview.
	if req.Command == "sync" || req.Command == "scrub" {
		if statusReport, err := s.engine.Status(ctx); err == nil {
			if statusSummary := scheduler.FormatStatusSummary(statusReport); statusSummary != "" {
				msg += "\n\n" + statusSummary
			}
		}
	}
	s.collector.EmitEvent("job_complete", "info", msg)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	values, err := agentdb.GetAllCacheValues(s.db)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, values)
}

// ConfigUpdateRequest is the payload for POST /api/config.
type ConfigUpdateRequest struct {
	Values map[string]string `json:"values"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var req ConfigUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	schedulerKeys := map[string]*string{
		"maintenance_cron": &s.cfg.Scheduler.MaintenanceCron,
		"scrub_cron":       &s.cfg.Scheduler.ScrubCron,
		"status_cron":      &s.cfg.Scheduler.StatusCron,
	}
	needReschedule := false

	for key, value := range req.Values {
		s.logger.Info("config value received", "key", key, "value", value)
		if err := agentdb.SetCacheValue(s.db, key, value); err != nil {
			s.logger.Error("failed to persist config value", "key", key, "error", err)
			addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist config"})
			return
		}
		if ptr, ok := schedulerKeys[key]; ok {
			*ptr = value
			needReschedule = true
		}
	}

	if needReschedule && s.sched != nil {
		s.logger.Info("rescheduling cron jobs after config update")
		if err := s.sched.Reschedule(s.schedCtx); err != nil {
			s.logger.Error("reschedule failed", "error", err)
		}
	}

	s.logger.Info("config updated", "keys_count", len(req.Values))
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Abort(); err != nil {
		status := http.StatusConflict
		if err == engine.ErrNoActiveJob {
			status = http.StatusNotFound
		}
		addonutil.WriteJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	s.logger.Info("active operation aborted via API")
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := agentdb.RecentJobs(s.db, 50)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, jobs)
}

// queryJobs returns job records filtered by the time_range query parameter.
func (s *Server) queryJobs(r *http.Request, limit int) ([]agentdb.JobRecord, error) {
	if tr := r.URL.Query().Get("time_range"); tr != "" {
		if d, ok := addonutil.ParseTimeRange(tr); ok {
			since := time.Now().UTC().Add(-d)
			return agentdb.RecentJobsSince(s.db, since, limit)
		}
	}
	return agentdb.RecentJobs(s.db, limit)
}

// handleJobDetail returns a single job record by ID, including the full output_log.
func (s *Server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid job id"})
		return
	}
	job, err := agentdb.GetJobByID(s.db, id)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if job == nil {
		addonutil.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, job)
}

// handleJobHistory returns job records with optional time_range filtering.
// Query params: time_range (e.g. "24h", "7d", "30d")
func (s *Server) handleJobHistory(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.queryJobs(r, 200)

	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if jobs == nil {
		jobs = []agentdb.JobRecord{}
	}
	// Strip output_log — it can be huge and the table doesn't display it.
	for i := range jobs {
		jobs[i].OutputLog = ""
	}
	addonutil.WriteJSON(w, http.StatusOK, jobs)
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
	jobs, err := s.queryJobs(r, 100)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var entries []LogEntry
	for _, j := range jobs {
		level := "info"
		switch j.Status {
		case "error", "failed":
			level = "error"
		case "running":
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
	addonutil.WriteJSON(w, http.StatusOK, entries)
}

// handleArrayStatus returns the cached disk status from the collector.
// If no cache exists, triggers a background refresh and returns empty immediately.
func (s *Server) handleArrayStatus(w http.ResponseWriter, r *http.Request) {
	report := s.collector.GetArrayStatus()
	if report == nil {
		// Trigger background refresh so data is available on next request.
		go s.backgroundRefreshStatus()
		addonutil.WriteJSON(w, http.StatusOK, []any{})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, report.DiskStatus)
}

// handleSmartStatus returns the cached SMART disk data from the collector.
// If no cache exists, triggers a background refresh and returns empty immediately.
func (s *Server) handleSmartStatus(w http.ResponseWriter, r *http.Request) {
	report := s.collector.GetSmartStatus()
	if report == nil {
		go s.backgroundRefreshSmart()
		addonutil.WriteJSON(w, http.StatusOK, []any{})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, report.Disks)
}

// handleActiveJob returns the current active job, or null if none.
func (s *Server) handleActiveJob(w http.ResponseWriter, r *http.Request) {
	job := s.collector.GetActiveJob()
	if job == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("null"))
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, job)
}

// handleDiskStorage returns OS-level filesystem info for snapraid data disks.
func (s *Server) handleDiskStorage(w http.ResponseWriter, r *http.Request) {
	storage := s.collector.GetDiskStorage()
	if storage == nil {
		// Trigger a collection so data is available on next request.
		go s.refreshDiskStorage()
		addonutil.WriteJSON(w, http.StatusOK, []any{})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, storage)
}

// refreshDiskStorage collects disk storage info without requiring the snapraid binary.
func (s *Server) refreshDiskStorage() {
	storage, err := s.engine.CollectDiskStorage()
	if err != nil {
		s.logger.Error("disk storage collection failed", "error", err)
		return
	}
	s.collector.SetDiskStorage(storage)
}

// backgroundRefresh guards a refresh function with an atomic flag so only one
// instance runs at a time. The name is used for log messages.
func (s *Server) backgroundRefresh(flag *atomic.Bool, name string, fn func()) {
	if !flag.CompareAndSwap(false, true) {
		return
	}
	defer flag.Store(false)

	s.logger.Info("background refresh: running " + name)
	fn()
	s.logger.Info("background refresh: " + name + " cache populated")
}

// backgroundRefreshStatus runs snapraid status in the background to populate the cache.
func (s *Server) backgroundRefreshStatus() {
	s.backgroundRefresh(&s.refreshingStatus, "status", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		report, err := s.engine.Status(ctx)
		if err != nil {
			s.logger.Error("background refresh status failed", "error", err)
			return
		}
		s.collector.SetArrayStatus(report)
		s.refreshDiskStorage()
	})
}

// backgroundRefreshSmart runs snapraid smart in the background to populate the cache.
func (s *Server) backgroundRefreshSmart() {
	s.backgroundRefresh(&s.refreshingSmart, "smart", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		report, err := s.engine.Smart(ctx)
		if err != nil {
			s.logger.Error("background refresh smart failed", "error", err)
			return
		}
		s.collector.SetSmartStatus(report)
	})
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
