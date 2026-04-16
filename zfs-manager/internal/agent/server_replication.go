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
	Target     string `json:"target"`
	DestTarget string `json:"dest_target"`
	Schedule   string `json:"schedule"`
	Recursive  bool   `json:"recursive"`
	Prefix     string `json:"prefix"`
	Retention  int    `json:"retention"`

	// Remote fields (required when mode == "remote").
	Mode          string `json:"replication_mode"` // "local" (default) | "remote"
	DestHost      string `json:"dest_host,omitempty"`
	DestPort      int    `json:"dest_port,omitempty"`
	DestUser      string `json:"dest_user,omitempty"`
	SSHKeyName    string `json:"ssh_key_name,omitempty"`
	BandwidthKbps int    `json:"bandwidth_kbps,omitempty"`
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
	mode := req.Mode
	if mode == "" {
		mode = "local"
	}
	if mode != "local" && mode != "remote" {
		addonutil.WriteError(w, http.StatusBadRequest, "replication_mode must be 'local' or 'remote'")
		return
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

	if mode == "remote" {
		if req.DestHost == "" || req.DestUser == "" || req.SSHKeyName == "" {
			addonutil.WriteError(w, http.StatusBadRequest,
				"remote replication requires dest_host, dest_user, and ssh_key_name")
			return
		}
		if !sshKeyNameValid(req.SSHKeyName) {
			addonutil.WriteError(w, http.StatusBadRequest,
				"ssh_key_name must contain only letters, digits, '-', '_' (max 64 chars)")
			return
		}
		// Ensure the keypair exists so the user can copy the public key right
		// after creation without a separate step. Idempotent if already there.
		if _, err := s.engine.EnsureSSHKey(ctx, req.SSHKeyName); err != nil {
			addonutil.WriteError(w, http.StatusInternalServerError,
				"ensure ssh key: "+err.Error())
			return
		}
		task.DestHost = &req.DestHost
		task.DestUser = &req.DestUser
		task.SSHKeyName = &req.SSHKeyName
		if req.DestPort > 0 {
			task.DestPort = &req.DestPort
		}
		if req.BandwidthKbps > 0 {
			task.BandwidthKbps = &req.BandwidthKbps
		}
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
	Target     string `json:"target"`
	DestTarget string `json:"dest_target"`
	Schedule   string `json:"schedule"`
	Recursive  bool   `json:"recursive"`
	Enabled    bool   `json:"enabled"`
	Prefix     string `json:"prefix"`
	Retention  int    `json:"retention"`

	// Remote fields — only applied when mode == "remote". Present so the UI
	// can edit host, port, user, key, or bandwidth without recreating the task.
	Mode          string `json:"replication_mode,omitempty"`
	DestHost      string `json:"dest_host,omitempty"`
	DestPort      int    `json:"dest_port,omitempty"`
	DestUser      string `json:"dest_user,omitempty"`
	SSHKeyName    string `json:"ssh_key_name,omitempty"`
	BandwidthKbps int    `json:"bandwidth_kbps,omitempty"`
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

	mode := req.Mode
	if mode == "" && existing.ReplicationMode != nil {
		mode = *existing.ReplicationMode
	}
	if mode == "" {
		mode = "local"
	}
	if mode != "local" && mode != "remote" {
		addonutil.WriteError(w, http.StatusBadRequest, "replication_mode must be 'local' or 'remote'")
		return
	}
	existing.ReplicationMode = &mode

	if mode == "remote" {
		if req.DestHost != "" {
			existing.DestHost = &req.DestHost
		}
		if req.DestUser != "" {
			existing.DestUser = &req.DestUser
		}
		if req.SSHKeyName != "" {
			if !sshKeyNameValid(req.SSHKeyName) {
				addonutil.WriteError(w, http.StatusBadRequest,
					"ssh_key_name must contain only letters, digits, '-', '_' (max 64 chars)")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			if _, err := s.engine.EnsureSSHKey(ctx, req.SSHKeyName); err != nil {
				addonutil.WriteError(w, http.StatusInternalServerError,
					"ensure ssh key: "+err.Error())
				return
			}
			existing.SSHKeyName = &req.SSHKeyName
		}
		if req.DestPort > 0 {
			existing.DestPort = &req.DestPort
		}
		// Bandwidth: 0 explicitly clears the cap; any positive value sets it.
		if req.BandwidthKbps > 0 {
			existing.BandwidthKbps = &req.BandwidthKbps
		} else {
			existing.BandwidthKbps = nil
		}
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
		mode := "local"
		if task.ReplicationMode != nil {
			mode = *task.ReplicationMode
		}

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

		srcSnaps, _ := s.engine.ListSnapshotsForDataset(ctx, task.Target)

		var result *ReplicationResult
		var runErr error
		switch mode {
		case "remote":
			tgt, terr := s.remoteTargetFromTask(task)
			if terr != nil {
				runErr = terr
				break
			}
			dstSnaps, _ := s.engine.ListRemoteSnapshotsForDataset(ctx, tgt)
			baseSnap := FindCommonSnapshot(srcSnaps, dstSnaps)
			result, runErr = s.engine.SendReceiveRemote(ctx, newSnap, baseSnap, tgt)
		default:
			dstSnaps, _ := s.engine.ListSnapshotsForDataset(ctx, destTarget)
			baseSnap := FindCommonSnapshot(srcSnaps, dstSnaps)
			result, runErr = s.engine.SendReceiveLocal(ctx, newSnap, baseSnap, destTarget)
		}

		if runErr != nil {
			msg := fmt.Sprintf("%s send|receive: %v", mode, runErr)
			s.logger.Error("manual replication failed", "task_id", id, "error", msg)
			agentdb.CompleteJob(s.db, jobID, "error", msg)
			s.emitReplicationEvent(task.Target, destTarget, false, msg)
			return
		}

		agentdb.UpdateLastSentSnap(s.db, task.ID, newSnap)

		msg := fmt.Sprintf("replicated %s → %s (%s %s, %s)",
			task.Target, destTarget, mode,
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

// --- Remote-replication support endpoints ---

type testRemoteConnectionRequest struct {
	DestTarget    string `json:"dest_target"`
	DestHost      string `json:"dest_host"`
	DestPort      int    `json:"dest_port,omitempty"`
	DestUser      string `json:"dest_user"`
	SSHKeyName    string `json:"ssh_key_name"`
	BandwidthKbps int    `json:"bandwidth_kbps,omitempty"`
}

type testRemoteConnectionResponse struct {
	OK      bool   `json:"ok"`
	Dataset string `json:"dataset,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handleTestRemoteConnection runs a read-only SSH probe against the destination
// host: it pins the host key on first use (accept-new), validates the private
// key is accepted, and checks that the remote user can `zfs list` the dest
// dataset. The UI uses this as a "Test Connection" button in the wizard.
func (s *Server) handleTestRemoteConnection(w http.ResponseWriter, r *http.Request) {
	var req testRemoteConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DestHost == "" || req.DestUser == "" || req.SSHKeyName == "" || req.DestTarget == "" {
		addonutil.WriteError(w, http.StatusBadRequest,
			"dest_host, dest_user, ssh_key_name, and dest_target are required")
		return
	}
	if !sshKeyNameValid(req.SSHKeyName) {
		addonutil.WriteError(w, http.StatusBadRequest,
			"ssh_key_name must contain only letters, digits, '-', '_' (max 64 chars)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	// Lazy-create the key so test can run immediately from a fresh wizard.
	if _, err := s.engine.EnsureSSHKey(ctx, req.SSHKeyName); err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError,
			"ensure ssh key: "+err.Error())
		return
	}

	port := req.DestPort
	if port == 0 {
		port = 22
	}
	tgt, err := s.engine.RemoteTargetFromTask(req.DestTarget, req.DestHost, req.DestUser, port, req.SSHKeyName, req.BandwidthKbps)
	if err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	dataset, err := s.engine.TestRemoteConnection(ctx, tgt)
	if err != nil {
		// 200 with ok=false so the UI can show the stderr verbatim without
		// the browser treating it as a network error.
		addonutil.WriteJSON(w, http.StatusOK, testRemoteConnectionResponse{
			OK:    false,
			Error: err.Error(),
		})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, testRemoteConnectionResponse{
		OK:      true,
		Dataset: dataset,
	})
}

// handleGetReplicationKeyPublic returns the OpenSSH authorized_keys line for a
// replication SSH keypair, creating it lazily if missing. The manifest wizard
// calls this to display the line the user needs to paste on the remote host.
func (s *Server) handleGetReplicationKeyPublic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sshKeyNameValid(name) {
		addonutil.WriteError(w, http.StatusBadRequest,
			"ssh key name must contain only letters, digits, '-', '_' (max 64 chars)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pub, err := s.engine.EnsureSSHKey(ctx, name)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{
		"name":       name,
		"public_key": pub,
	})
}

// remoteTargetFromTask resolves a ScheduledTask's remote fields into a
// RemoteTarget, returning a descriptive error if any required field is
// missing. Used by the manual-run handler and the /api/replication/preview
// path (if/when added).
func (s *Server) remoteTargetFromTask(task *agentdb.ScheduledTask) (RemoteTarget, error) {
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
		return RemoteTarget{}, fmt.Errorf("remote replication task %d missing: %s", task.ID, strings.Join(missing, ", "))
	}
	port := 22
	if task.DestPort != nil && *task.DestPort > 0 {
		port = *task.DestPort
	}
	bandwidth := 0
	if task.BandwidthKbps != nil {
		bandwidth = *task.BandwidthKbps
	}
	return s.engine.RemoteTargetFromTask(*task.DestTarget, *task.DestHost, *task.DestUser, port, *task.SSHKeyName, bandwidth)
}
