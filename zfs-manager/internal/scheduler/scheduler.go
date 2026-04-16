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
	case "replication":
		err = s.runReplicationTask(ctx, task)
	default:
		err = fmt.Errorf("unknown task type: %s", task.TaskType)
	}

	if err != nil {
		s.logger.Error("scheduled task failed", "task_id", task.ID, "error", err)
		agentdb.CompleteJob(s.db, jobID, "error", err.Error())

		// Emit failure event for replication tasks.
		if task.TaskType == "replication" {
			dest := ""
			if task.DestTarget != nil {
				dest = *task.DestTarget
			}
			s.collector.EmitEvent(agent.AgentEvent{
				ID:        fmt.Sprintf("repl-fail-%d-%d", task.ID, time.Now().UnixMilli()),
				Type:      "replication_failed",
				Severity:  "critical",
				Message:   fmt.Sprintf("replication %s → %s failed: %s", task.Target, dest, err),
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
		}
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

// runReplicationTask performs a zfs send|receive for a scheduled replication
// task — either local (two datasets on this host) or remote (over SSH).
// Flow: create fresh source snapshot → resolve common snapshot → send|receive
// → update bookkeeping → retention on both sides.
func (s *Scheduler) runReplicationTask(ctx context.Context, task agentdb.ScheduledTask) error {
	if task.DestTarget == nil || *task.DestTarget == "" {
		return fmt.Errorf("replication task %d has no dest_target", task.ID)
	}
	destTarget := *task.DestTarget
	mode := "local"
	if task.ReplicationMode != nil {
		mode = *task.ReplicationMode
	}

	// 1. Create a fresh snapshot on the source so we always have something to send.
	snapName := task.Prefix + "-" + time.Now().UTC().Format("2006-01-02-150405")
	if _, err := s.engine.CreateSnapshot(ctx, task.Target, snapName, task.Recursive); err != nil {
		return fmt.Errorf("create source snapshot %s@%s: %w", task.Target, snapName, err)
	}
	newSnap := task.Target + "@" + snapName
	s.logger.Info("replication: source snapshot created", "snap", newSnap, "mode", mode)

	// 2. Resolve the common snapshot for incremental send. Destination listing
	//    is local for local replication and SSH for remote.
	srcSnaps, err := s.engine.ListSnapshotsForDataset(ctx, task.Target)
	if err != nil {
		return fmt.Errorf("list source snapshots: %w", err)
	}

	var result *agent.ReplicationResult
	var baseSnap string

	switch mode {
	case "remote":
		tgt, err := remoteTargetFromTask(s.engine, task)
		if err != nil {
			return err
		}
		dstSnaps, _ := s.engine.ListRemoteSnapshotsForDataset(ctx, tgt)
		baseSnap = agent.FindCommonSnapshot(srcSnaps, dstSnaps)
		s.logSendDecision(mode, baseSnap, newSnap)

		result, err = s.engine.SendReceiveRemote(ctx, newSnap, baseSnap, tgt)
		if err != nil {
			return fmt.Errorf("remote send|receive: %w", err)
		}

	default: // "local" (or empty, back-compat)
		dstSnaps, _ := s.engine.ListSnapshotsForDataset(ctx, destTarget)
		baseSnap = agent.FindCommonSnapshot(srcSnaps, dstSnaps)
		s.logSendDecision(mode, baseSnap, newSnap)

		result, err = s.engine.SendReceiveLocal(ctx, newSnap, baseSnap, destTarget)
		if err != nil {
			return fmt.Errorf("local send|receive: %w", err)
		}
	}

	msg := fmt.Sprintf("replicated %s → %s (%s, %s)",
		task.Target, destTarget, mode, result.Duration.Round(time.Second))
	s.logger.Info("replication: completed",
		"snap", newSnap, "incremental", result.Incremental, "mode", mode,
		"duration", result.Duration.Round(time.Second))

	// Emit success event for upstream notifications.
	s.collector.EmitEvent(agent.AgentEvent{
		ID:        fmt.Sprintf("repl-%s-%d", task.Target, time.Now().UnixMilli()),
		Type:      "replication_succeeded",
		Severity:  "info",
		Message:   msg,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})

	// 4. Update last_sent_snap bookkeeping.
	if err := agentdb.UpdateLastSentSnap(s.db, task.ID, newSnap); err != nil {
		s.logger.Error("replication: update last_sent_snap failed", "error", err)
	}

	// 5. Apply retention on source (always local).
	if task.Retention > 0 {
		if err := s.applyRetention(ctx, task); err != nil {
			s.logger.Error("replication: source retention failed", "error", err)
		}
	}

	// 6. Apply retention on destination. Only local mode — remote retention is
	//    out of v0.5.0 scope (would need a second SSH round-trip with destroy
	//    permissions the remote user may not have).
	if mode == "local" && task.Retention > 0 {
		if err := s.applyRetentionOnDataset(ctx, destTarget, task.Prefix, task.Retention); err != nil {
			s.logger.Error("replication: dest retention failed", "error", err)
		}
	}

	go s.collector.RequestFlush()
	return nil
}

func (s *Scheduler) logSendDecision(mode, baseSnap, newSnap string) {
	if baseSnap != "" {
		s.logger.Info("replication: incremental send", "base", baseSnap, "snap", newSnap, "mode", mode)
	} else {
		s.logger.Info("replication: full send (no common snapshot)", "snap", newSnap, "mode", mode)
	}
}

// remoteTargetFromTask resolves a ScheduledTask into a RemoteTarget, returning
// a descriptive error if any required remote field is missing.
func remoteTargetFromTask(engine *agent.Engine, task agentdb.ScheduledTask) (agent.RemoteTarget, error) {
	var missing []string
	if task.DestHost == nil || *task.DestHost == "" {
		missing = append(missing, "dest_host")
	}
	if task.DestUser == nil || *task.DestUser == "" {
		missing = append(missing, "dest_user")
	}
	if task.SSHKeyName == nil || *task.SSHKeyName == "" {
		missing = append(missing, "ssh_key_name")
	}
	if task.DestTarget == nil || *task.DestTarget == "" {
		missing = append(missing, "dest_target")
	}
	if len(missing) > 0 {
		return agent.RemoteTarget{}, fmt.Errorf("remote replication task %d missing: %s", task.ID, strings.Join(missing, ", "))
	}
	port := 22
	if task.DestPort != nil && *task.DestPort > 0 {
		port = *task.DestPort
	}
	bandwidth := 0
	if task.BandwidthKbps != nil {
		bandwidth = *task.BandwidthKbps
	}
	return engine.RemoteTargetFromTask(*task.DestTarget, *task.DestHost, *task.DestUser, port, *task.SSHKeyName, bandwidth)
}

// applyRetentionOnDataset is a generalized retention helper that works on any
// dataset — used for the destination side of replication where the dataset
// differs from task.Target.
func (s *Scheduler) applyRetentionOnDataset(ctx context.Context, dataset, prefix string, retention int) error {
	snapshots, err := s.engine.ListSnapshotsForDataset(ctx, dataset)
	if err != nil {
		return fmt.Errorf("list snapshots for retention on %s: %w", dataset, err)
	}

	var matching []agent.SnapshotInfo
	for _, snap := range snapshots {
		if strings.HasPrefix(snap.SnapName, prefix+"-") {
			matching = append(matching, snap)
		}
	}

	if len(matching) <= retention {
		return nil
	}

	// Already sorted by creation ascending from ListSnapshotsForDataset.
	toDelete := matching[:len(matching)-retention]
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
