package scheduler

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	"github.com/pineappledr/vigil-addons/snapraid/internal/engine"
	"github.com/robfig/cron/v3"
)

// Scheduler manages recurring SnapRAID jobs via cron expressions.
type Scheduler struct {
	cron   *cron.Cron
	engine *engine.Engine
	cfg    *config.AgentConfig
	db     *sql.DB
	logger *slog.Logger

	// jobMu serializes scheduled job execution so only one pipeline runs at a time,
	// even if cron fires overlap. The engine mutex guards the binary itself;
	// this mutex guards the scheduler's pipeline sequencing.
	jobMu sync.Mutex
}

// New creates a Scheduler wired to the given engine, config, and database.
func New(eng *engine.Engine, cfg *config.AgentConfig, database *sql.DB, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		cron:   cron.New(cron.WithSeconds()),
		engine: eng,
		cfg:    cfg,
		db:     database,
		logger: logger,
	}
}

// Start registers all configured cron schedules and begins the cron runner.
func (s *Scheduler) Start(ctx context.Context) error {
	schedules := []struct {
		expr string
		name string
		fn   func(context.Context)
	}{
		{s.cfg.Scheduler.MaintenanceCron, "maintenance", s.runMaintenance},
		{s.cfg.Scheduler.ScrubCron, "scrub_only", s.runScrubOnly},
		{s.cfg.Scheduler.SmartCron, "smart_check", s.runSmartCheck},
		{s.cfg.Scheduler.StatusCron, "status_refresh", s.runStatusRefresh},
	}

	// robfig/cron/v3 with WithSeconds() expects 6-field expressions.
	// Our config uses standard 5-field cron, so we prepend "0 " for the seconds field.
	for _, sched := range schedules {
		if sched.expr == "" {
			continue
		}
		expr := "0 " + sched.expr
		name := sched.name
		fn := sched.fn
		_, err := s.cron.AddFunc(expr, func() {
			s.logger.Info("cron triggered", "job", name)
			fn(ctx)
		})
		if err != nil {
			return &CronRegistrationError{Job: name, Expr: sched.expr, Err: err}
		}
		s.logger.Info("registered cron schedule", "job", name, "expr", sched.expr)
	}

	s.cron.Start()
	return nil
}

// Stop gracefully shuts down the cron scheduler, waiting for running jobs to finish.
func (s *Scheduler) Stop() context.Context {
	return s.cron.Stop()
}

// runMaintenance executes the full maintenance pipeline under the job mutex.
func (s *Scheduler) runMaintenance(ctx context.Context) {
	if !s.jobMu.TryLock() {
		s.logger.Warn("maintenance skipped: previous scheduled job still running")
		return
	}
	defer s.jobMu.Unlock()

	p := &Pipeline{
		engine: s.engine,
		cfg:    s.cfg,
		db:     s.db,
		logger: s.logger,
	}
	p.RunMaintenance(ctx)
}

// runScrubOnly executes a standalone scrub job.
func (s *Scheduler) runScrubOnly(ctx context.Context) {
	if !s.jobMu.TryLock() {
		s.logger.Warn("scrub_only skipped: previous scheduled job still running")
		return
	}
	defer s.jobMu.Unlock()

	p := &Pipeline{
		engine: s.engine,
		cfg:    s.cfg,
		db:     s.db,
		logger: s.logger,
	}
	p.RunScrubOnly(ctx)
}

// runSmartCheck executes a standalone SMART check.
func (s *Scheduler) runSmartCheck(ctx context.Context) {
	if !s.jobMu.TryLock() {
		s.logger.Warn("smart_check skipped: previous scheduled job still running")
		return
	}
	defer s.jobMu.Unlock()

	p := &Pipeline{
		engine: s.engine,
		cfg:    s.cfg,
		db:     s.db,
		logger: s.logger,
	}
	p.RunSmartCheck(ctx)
}

// runStatusRefresh executes a standalone status refresh.
func (s *Scheduler) runStatusRefresh(ctx context.Context) {
	if !s.jobMu.TryLock() {
		s.logger.Warn("status_refresh skipped: previous scheduled job still running")
		return
	}
	defer s.jobMu.Unlock()

	p := &Pipeline{
		engine: s.engine,
		cfg:    s.cfg,
		db:     s.db,
		logger: s.logger,
	}
	p.RunStatusRefresh(ctx)
}

// CronRegistrationError describes a failure to register a cron schedule.
type CronRegistrationError struct {
	Job  string
	Expr string
	Err  error
}

func (e *CronRegistrationError) Error() string {
	return "failed to register cron for " + e.Job + " (" + e.Expr + "): " + e.Err.Error()
}

func (e *CronRegistrationError) Unwrap() error {
	return e.Err
}
