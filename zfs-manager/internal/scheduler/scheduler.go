package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pineappledr/vigil-addons/zfs-manager/internal/agent"
	agentdb "github.com/pineappledr/vigil-addons/zfs-manager/internal/db"
	"github.com/robfig/cron/v3"
)

// Scheduler manages periodic snapshot and scrub tasks via cron.
type Scheduler struct {
	cron      *cron.Cron
	engine    *agent.Engine
	collector *agent.Collector
	db        *sql.DB
	logger    *slog.Logger

	// jobMu serializes scheduled job execution.
	jobMu sync.Mutex
}

// New creates a Scheduler.
func New(engine *agent.Engine, collector *agent.Collector, database *sql.DB, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		cron:      cron.New(cron.WithSeconds(), cron.WithLocation(time.Local)),
		engine:    engine,
		collector: collector,
		db:        database,
		logger:    logger,
	}
}

// Start loads all enabled tasks from the DB and registers them with cron.
func (s *Scheduler) Start(ctx context.Context) error {
	tasks, err := agentdb.ListEnabledTasks(s.db)
	if err != nil {
		return fmt.Errorf("load tasks: %w", err)
	}

	for _, task := range tasks {
		if err := s.registerTask(ctx, task); err != nil {
			s.logger.Error("failed to register task", "task_id", task.ID, "error", err)
		}
	}

	s.cron.Start()
	s.logger.Info("scheduler started", "tasks", len(tasks))
	return nil
}

// Stop gracefully stops the scheduler.
func (s *Scheduler) Stop() context.Context {
	return s.cron.Stop()
}

// Reload stops, clears, and re-registers all enabled tasks.
func (s *Scheduler) Reload(ctx context.Context) error {
	<-s.cron.Stop().Done()
	s.cron = cron.New(cron.WithSeconds(), cron.WithLocation(time.Local))
	return s.Start(ctx)
}

// NextRunTimes returns the next scheduled run time for each task ID.
func (s *Scheduler) NextRunTimes() map[int64]time.Time {
	result := make(map[int64]time.Time)
	for _, entry := range s.cron.Entries() {
		if taskID, ok := entry.Job.(*taskJob); ok {
			result[taskID.taskID] = entry.Next
		}
	}
	return result
}

// registerTask adds a single task to the cron scheduler.
func (s *Scheduler) registerTask(ctx context.Context, task agentdb.ScheduledTask) error {
	// robfig/cron/v3 with WithSeconds() expects 6-field; prepend "0 " for seconds.
	expr := "0 " + task.Schedule
	job := &taskJob{
		scheduler: s,
		taskID:    task.ID,
		task:      task,
		ctx:       ctx,
	}
	_, err := s.cron.AddJob(expr, job)
	if err != nil {
		return fmt.Errorf("register cron for task %d (%s): %w", task.ID, task.Schedule, err)
	}
	s.logger.Info("registered scheduled task", "task_id", task.ID, "type", task.TaskType, "target", task.Target, "schedule", task.Schedule)
	return nil
}

// taskJob implements cron.Job for a scheduled task.
type taskJob struct {
	scheduler *Scheduler
	taskID    int64
	task      agentdb.ScheduledTask
	ctx       context.Context
}

func (j *taskJob) Run() {
	j.scheduler.executeTask(j.ctx, j.task)
}

// executeTask runs a snapshot or scrub task.
func (s *Scheduler) executeTask(ctx context.Context, task agentdb.ScheduledTask) {
	if !s.jobMu.TryLock() {
		s.logger.Warn("scheduled task skipped: previous job still running", "task_id", task.ID)
		return
	}
	defer s.jobMu.Unlock()

	taskID := task.ID
	jobID, _ := agentdb.InsertJob(s.db, &taskID, task.TaskType, "scheduled")

	s.logger.Info("executing scheduled task", "task_id", task.ID, "type", task.TaskType, "target", task.Target)

	var err error
	switch task.TaskType {
	case "snapshot":
		err = s.runSnapshotTask(ctx, task)
	case "scrub":
		err = s.runScrubTask(ctx, task)
	default:
		err = fmt.Errorf("unknown task type: %s", task.TaskType)
	}

	if err != nil {
		s.logger.Error("scheduled task failed", "task_id", task.ID, "error", err)
		agentdb.CompleteJob(s.db, jobID, "error", err.Error())
		return
	}

	agentdb.CompleteJob(s.db, jobID, "success", "")
	s.logger.Info("scheduled task completed", "task_id", task.ID)
}

// runSnapshotTask takes a snapshot and applies retention.
func (s *Scheduler) runSnapshotTask(ctx context.Context, task agentdb.ScheduledTask) error {
	snapName := task.Prefix + "-" + time.Now().UTC().Format("2006-01-02-150405")

	result, err := s.engine.CreateSnapshot(ctx, task.Target, snapName, task.Recursive)
	if err != nil {
		return fmt.Errorf("create snapshot %s@%s: %w (output: %s)", task.Target, snapName, err, result.Output)
	}

	s.logger.Info("snapshot created", "dataset", task.Target, "snap", snapName)

	// Apply retention policy
	if task.Retention > 0 {
		if err := s.applyRetention(ctx, task); err != nil {
			s.logger.Error("retention cleanup failed", "task_id", task.ID, "error", err)
			// Don't fail the task — the snapshot was taken successfully
		}
	}

	// Refresh telemetry
	go s.collector.RequestFlush()
	return nil
}

// runScrubTask starts a scrub on the target pool.
func (s *Scheduler) runScrubTask(ctx context.Context, task agentdb.ScheduledTask) error {
	result, err := s.engine.StartScrub(ctx, task.Target)
	if err != nil {
		return fmt.Errorf("scrub %s: %w (output: %s)", task.Target, err, result.Output)
	}

	s.logger.Info("scrub started", "pool", task.Target)
	go s.collector.RequestFlush()
	return nil
}

// applyRetention deletes the oldest snapshots matching the task prefix until
// only `task.Retention` snapshots remain.
func (s *Scheduler) applyRetention(ctx context.Context, task agentdb.ScheduledTask) error {
	snapshots, err := s.engine.ListSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("list snapshots for retention: %w", err)
	}

	// Filter to snapshots matching this task's dataset and prefix
	var matching []agent.SnapshotInfo
	for _, snap := range snapshots {
		if snap.Dataset == task.Target && strings.HasPrefix(snap.SnapName, task.Prefix+"-") {
			matching = append(matching, snap)
		}
	}

	if len(matching) <= task.Retention {
		return nil
	}

	// Sort by creation ascending (oldest first)
	sort.Slice(matching, func(i, j int) bool {
		return matching[i].Creation < matching[j].Creation
	})

	toDelete := matching[:len(matching)-task.Retention]
	for _, snap := range toDelete {
		result, err := s.engine.DestroySnapshot(ctx, snap.FullName)
		if err != nil {
			s.logger.Error("retention delete failed", "snapshot", snap.FullName, "error", err, "output", result.Output)
			continue
		}
		s.logger.Info("retention: deleted snapshot", "snapshot", snap.FullName)
	}

	return nil
}
