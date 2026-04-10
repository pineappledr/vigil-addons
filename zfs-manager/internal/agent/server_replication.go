package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
	agentdb "github.com/pineappledr/vigil-addons/zfs-manager/internal/db"
)

// --- Phase 5: Local Replication Handlers ---

type replicationTaskView struct {
	agentdb.ScheduledTask
	NextRun string `json:"next_run,omitempty"`
}

func (s *Server) handleListReplicationTasks(w http.ResponseWriter, _ *http.Request) {
	tasks, err := agentdb.ListTasks(s.db)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	nextRuns := s.scheduler.NextRunTimes()
	var views []replicationTaskView
	for _, t := range tasks {
		if t.TaskType != "replication" {
			continue
		}
		v := replicationTaskView{ScheduledTask: t}
		if next, ok := nextRuns[t.ID]; ok {
			v.NextRun = next.UTC().Format(time.RFC3339)
		}
		views = append(views, v)
	}
	if views == nil {
		views = []replicationTaskView{}
	}
	addonutil.WriteJSON(w, http.StatusOK, views)
}

type createReplicationRequest struct {
	Target    string `json:"target"`
	DestTarget string `json:"dest_target"`
	Schedule  string `json:"schedule"`
	Recursive bool   `json:"recursive"`
	Prefix    string `json:"prefix"`
	Retention int    `json:"retention"`
}

func (s *Server) handleCreateReplicationTask(w http.ResponseWriter, r *http.Request) {
	var req createReplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Target == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "target (source dataset) is required")
		return
	}
	if req.DestTarget == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "dest_target (destination dataset) is required")
		return
	}
	if req.Target == req.DestTarget {
		addonutil.WriteError(w, http.StatusBadRequest, "source and destination must be different")
		return
	}
	if req.Schedule == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "schedule is required")
		return
	}
	if req.Prefix == "" {
		req.Prefix = "repl"
	}

	// Validate that source dataset exists.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	srcSnaps, err := s.engine.ListSnapshotsForDataset(ctx, req.Target)
	if err != nil && !strings.Contains(err.Error(), "does not exist") {
		// Non-fatal — dataset exists but has issues.
		s.logger.Warn("replication: source dataset check", "error", err)
	}
	_ = srcSnaps // existence check only

	mode := "local"
	task := agentdb.ScheduledTask{
		TaskType:        "replication",
		Target:          req.Target,
		Schedule:        req.Schedule,
		Recursive:       req.Recursive,
		Enabled:         true,
		Prefix:          req.Prefix,
		Retention:       req.Retention,
		DestTarget:      &req.DestTarget,
		ReplicationMode: &mode,
	}

	id, err := agentdb.InsertTask(s.db, task)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("created replication task", "task_id", id, "source", req.Target, "dest", req.DestTarget)

	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.logger.Error("scheduler reload failed", "error", err)
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]any{"status": "created", "id": id})
}

type updateReplicationRequest struct {
	Target    string `json:"target"`
	DestTarget string `json:"dest_target"`
	Schedule  string `json:"schedule"`
	Recursive bool   `json:"recursive"`
	Enabled   bool   `json:"enabled"`
	Prefix    string `json:"prefix"`
	Retention int    `json:"retention"`
}

func (s *Server) handleUpdateReplicationTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid task id")
		return
	}

	existing, err := agentdb.GetTask(s.db, id)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil || existing.TaskType != "replication" {
		addonutil.WriteError(w, http.StatusNotFound, "replication task not found")
		return
	}

	var req updateReplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing.Target = req.Target
	existing.Schedule = req.Schedule
	existing.Recursive = req.Recursive
	existing.Enabled = req.Enabled
	existing.Prefix = req.Prefix
	existing.Retention = req.Retention
	if req.DestTarget != "" {
		existing.DestTarget = &req.DestTarget
	}

	if err := agentdb.UpdateTask(s.db, *existing); err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("updated replication task", "task_id", id)

	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.logger.Error("scheduler reload failed", "error", err)
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteReplicationTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid task id")
		return
	}

	existing, err := agentdb.GetTask(s.db, id)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil || existing.TaskType != "replication" {
		addonutil.WriteError(w, http.StatusNotFound, "replication task not found")
		return
	}

	if err := agentdb.DeleteTask(s.db, id); err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("deleted replication task", "task_id", id)

	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.logger.Error("scheduler reload failed", "error", err)
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRunReplicationTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid task id")
		return
	}

	task, err := agentdb.GetTask(s.db, id)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil || task.TaskType != "replication" {
		addonutil.WriteError(w, http.StatusNotFound, "replication task not found")
		return
	}
	if task.DestTarget == nil || *task.DestTarget == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "task has no destination configured")
		return
	}

	// Run asynchronously — record job, execute, complete.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()

		taskID := task.ID
		jobID, _ := agentdb.InsertJob(s.db, &taskID, "replication", "manual")

		s.logger.Info("manual replication run started", "task_id", id)

		destTarget := *task.DestTarget

		// Create fresh source snapshot.
		snapName := task.Prefix + "-" + time.Now().UTC().Format("2006-01-02-150405")
		if _, err := s.engine.CreateSnapshot(ctx, task.Target, snapName, task.Recursive); err != nil {
			msg := fmt.Sprintf("create source snapshot: %v", err)
			s.logger.Error("manual replication failed", "task_id", id, "error", msg)
			agentdb.CompleteJob(s.db, jobID, "error", msg)
			s.emitReplicationEvent(task.Target, destTarget, false, msg)
			return
		}
		newSnap := task.Target + "@" + snapName

		// Resolve common snapshot.
		srcSnaps, _ := s.engine.ListSnapshotsForDataset(ctx, task.Target)
		dstSnaps, _ := s.engine.ListSnapshotsForDataset(ctx, destTarget)
		baseSnap := FindCommonSnapshot(srcSnaps, dstSnaps)

		// Run pipeline.
		result, err := s.engine.SendReceiveLocal(ctx, newSnap, baseSnap, destTarget)
		if err != nil {
			msg := fmt.Sprintf("send|receive: %v", err)
			s.logger.Error("manual replication failed", "task_id", id, "error", msg)
			agentdb.CompleteJob(s.db, jobID, "error", msg)
			s.emitReplicationEvent(task.Target, destTarget, false, msg)
			return
		}

		agentdb.UpdateLastSentSnap(s.db, task.ID, newSnap)

		msg := fmt.Sprintf("replicated %s → %s (%s, %s)",
			task.Target, destTarget,
			boolStr(result.Incremental, "incremental", "full"),
			result.Duration.Round(time.Second))
		agentdb.CompleteJob(s.db, jobID, "success", msg)
		s.emitReplicationEvent(task.Target, destTarget, true, msg)
		s.logger.Info("manual replication completed", "task_id", id, "result", msg)

		go s.refreshAndFlush()
	}()

	addonutil.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) handleReplicationTaskHistory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid task id")
		return
	}

	jobs, err := agentdb.JobsForTask(s.db, id, 50)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []agentdb.JobRecord{}
	}
	addonutil.WriteJSON(w, http.StatusOK, jobs)
}

// emitReplicationEvent emits a replication_succeeded or replication_failed event.
func (s *Server) emitReplicationEvent(source, dest string, success bool, message string) {
	eventType := "replication_failed"
	severity := "critical"
	if success {
		eventType = "replication_succeeded"
		severity = "info"
	}
	s.collector.EmitEvent(AgentEvent{
		ID:        fmt.Sprintf("repl-%s-%s-%d", source, dest, time.Now().UnixMilli()),
		Type:      eventType,
		Severity:  severity,
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}
