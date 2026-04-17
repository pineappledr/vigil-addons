package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
	"github.com/pineappledr/vigil-addons/zfs-manager/internal/config"
	agentdb "github.com/pineappledr/vigil-addons/zfs-manager/internal/db"
)

// SchedulerReloader can reload cron schedules after task changes.
type SchedulerReloader interface {
	Reload(ctx context.Context) error
	NextRunTimes() map[int64]time.Time
}

// Server is the Agent HTTP server.
type Server struct {
	cfg       *config.AgentConfig
	engine    *Engine
	collector *Collector
	db        *sql.DB
	scheduler SchedulerReloader
	mux       *http.ServeMux
	server    *http.Server
	logger    *slog.Logger
}

// NewServer creates the Agent server.
func NewServer(cfg *config.AgentConfig, engine *Engine, collector *Collector, database *sql.DB, sched SchedulerReloader, logger *slog.Logger) *Server {
	s := &Server{
		cfg:       cfg,
		engine:    engine,
		collector: collector,
		db:        database,
		scheduler: sched,
		mux:       http.NewServeMux(),
		logger:    logger,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// Read-only (telemetry)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/telemetry", s.handleTelemetry)
	s.mux.HandleFunc("GET /api/pools", s.handlePools)
	s.mux.HandleFunc("GET /api/datasets", s.handleDatasets)
	s.mux.HandleFunc("GET /api/snapshots", s.handleSnapshots)
	s.mux.HandleFunc("GET /api/presets", s.handlePresets)
	s.mux.HandleFunc("GET /api/arc", s.handleARC)
	s.mux.HandleFunc("GET /api/iostat", s.handleIOStat)

	// Write operations (Phase 2)
	s.mux.HandleFunc("POST /api/datasets", s.handleCreateDataset)
	s.mux.HandleFunc("PUT /api/datasets", s.handleEditDataset)
	s.mux.HandleFunc("DELETE /api/datasets", s.handleDeleteDataset)
	s.mux.HandleFunc("POST /api/snapshots", s.handleCreateSnapshot)
	s.mux.HandleFunc("DELETE /api/snapshots", s.handleDeleteSnapshot)
	s.mux.HandleFunc("POST /api/snapshots/rollback", s.handleRollbackSnapshot)
	s.mux.HandleFunc("POST /api/scrub/start", s.handleStartScrub)
	s.mux.HandleFunc("POST /api/scrub/pause", s.handlePauseScrub)
	s.mux.HandleFunc("POST /api/scrub/cancel", s.handleCancelScrub)

	// Command preview (returns the CLI command without executing)
	s.mux.HandleFunc("POST /api/preview", s.handlePreview)

	// Phase 4 — Disk & Pool Operations
	s.mux.HandleFunc("GET /api/disks", s.handleListDisks)
	s.mux.HandleFunc("POST /api/pool/replace", s.handleReplaceDevice)
	s.mux.HandleFunc("POST /api/pool/add-vdev", s.handleAddVdev)
	s.mux.HandleFunc("POST /api/devices/offline", s.handleOfflineDevice)
	s.mux.HandleFunc("POST /api/devices/online", s.handleOnlineDevice)
	s.mux.HandleFunc("POST /api/devices/identify", s.handleIdentifyDevice)
	s.mux.HandleFunc("POST /api/pool/clear", s.handleClearErrors)

	// Phase 3 — Scheduled Tasks
	s.mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	s.mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	s.mux.HandleFunc("PUT /api/tasks/{id}", s.handleUpdateTask)
	s.mux.HandleFunc("DELETE /api/tasks/{id}", s.handleDeleteTask)
	s.mux.HandleFunc("GET /api/tasks/{id}/history", s.handleTaskHistory)
	s.mux.HandleFunc("GET /api/jobs", s.handleJobHistory)
	s.mux.HandleFunc("GET /api/retention", s.handleRetentionStats)
	s.mux.HandleFunc("POST /api/retention/cleanup", s.handleRetentionCleanup)

	// Phase 5 — Replication (local + remote)
	s.mux.HandleFunc("GET /api/replication/tasks", s.handleListReplicationTasks)
	s.mux.HandleFunc("POST /api/replication/tasks", s.handleCreateReplicationTask)
	s.mux.HandleFunc("PUT /api/replication/tasks/{id}", s.handleUpdateReplicationTask)
	s.mux.HandleFunc("DELETE /api/replication/tasks/{id}", s.handleDeleteReplicationTask)
	s.mux.HandleFunc("POST /api/replication/tasks/{id}/run", s.handleRunReplicationTask)
	s.mux.HandleFunc("GET /api/replication/tasks/{id}/history", s.handleReplicationTaskHistory)
	s.mux.HandleFunc("POST /api/replication/test-connection", s.handleTestRemoteConnection)
	s.mux.HandleFunc("GET /api/replication/keys/{name}/public", s.handleGetReplicationKeyPublic)
	s.mux.HandleFunc("POST /api/replication/keys/{name}/rotate", s.handleRotateReplicationKey)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleTelemetry(w http.ResponseWriter, _ *http.Request) {
	payload := s.collector.Build()
	addonutil.WriteJSON(w, http.StatusOK, payload)
}

func (s *Server) handlePools(w http.ResponseWriter, _ *http.Request) {
	pools := s.collector.GetPools()
	if pools == nil {
		pools = []PoolInfo{}
	}
	addonutil.WriteJSON(w, http.StatusOK, pools)
}

func (s *Server) handleDatasets(w http.ResponseWriter, _ *http.Request) {
	datasets := s.collector.GetDatasets()
	if datasets == nil {
		datasets = []DatasetInfo{}
	}
	addonutil.WriteJSON(w, http.StatusOK, datasets)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, _ *http.Request) {
	snapshots := s.collector.GetSnapshots()
	if snapshots == nil {
		snapshots = []SnapshotInfo{}
	}
	addonutil.WriteJSON(w, http.StatusOK, snapshots)
}

func (s *Server) handlePresets(w http.ResponseWriter, _ *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, DatasetPresets)
}

// handleIOStat returns the most recent per-pool iostat counters. Values are
// cumulative since pool import; rate computation is hub-side.
func (s *Server) handleIOStat(w http.ResponseWriter, _ *http.Request) {
	stats := s.collector.GetIOStat()
	if stats == nil {
		stats = []PoolIOStat{}
	}
	addonutil.WriteJSON(w, http.StatusOK, stats)
}

// handleARC returns the most recent ARC/L2ARC snapshot. On hosts without
// /proc/spl/kstat/zfs/arcstats (non-Linux, zfs.ko not loaded), responds 501
// so the UI can render a dedicated "not supported" state rather than an
// empty dashboard that looks broken.
func (s *Server) handleARC(w http.ResponseWriter, _ *http.Request) {
	if arc := s.collector.GetARC(); arc != nil {
		addonutil.WriteJSON(w, http.StatusOK, arc)
		return
	}
	if !ARCStatsAvailable() {
		addonutil.WriteError(w, http.StatusNotImplemented,
			"arcstats are not available on this host — /proc/spl/kstat/zfs/arcstats does not exist")
		return
	}
	// Capability is present but no snapshot has landed yet (collector hasn't
	// run its first Refresh). Return a 204 so the UI polls again shortly.
	w.WriteHeader(http.StatusNoContent)
}

// --- Dataset Management ---

type createDatasetRequest struct {
	Parent      string            `json:"parent"`
	Name        string            `json:"name"`
	Preset      string            `json:"preset,omitempty"`
	Properties  map[string]string `json:"properties,omitempty"`
	Quota       string            `json:"quota,omitempty"`
	Reservation string            `json:"reservation,omitempty"`
}

var validDatasetName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func (s *Server) handleCreateDataset(w http.ResponseWriter, r *http.Request) {
	var req createDatasetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Parent == "" || req.Name == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "parent and name are required")
		return
	}
	if !validDatasetName.MatchString(req.Name) {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid dataset name: only alphanumeric, dots, hyphens, underscores allowed")
		return
	}

	fullName := req.Parent + "/" + req.Name

	props := make(map[string]string)
	// Apply preset if specified
	if preset, ok := DatasetPresets[req.Preset]; ok {
		props["recordsize"] = preset.RecordSize
		props["compression"] = preset.Compression
		props["atime"] = preset.Atime
		props["sync"] = preset.Sync
	}
	// Override with explicit properties
	for k, v := range req.Properties {
		props[k] = v
	}
	if req.Quota != "" {
		props["quota"] = req.Quota
	}
	if req.Reservation != "" {
		props["reservation"] = req.Reservation
	}

	s.logger.Info("creating dataset", "name", fullName, "props", props)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.CreateDataset(ctx, fullName, props)
	if err != nil {
		s.logger.Error("create dataset failed", "name", fullName, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	// Refresh telemetry cache after write operation
	go s.refreshAndFlush()

	addonutil.WriteJSON(w, http.StatusOK, result)
}

type editDatasetRequest struct {
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties"`
}

func (s *Server) handleEditDataset(w http.ResponseWriter, r *http.Request) {
	var req editDatasetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || len(req.Properties) == 0 {
		addonutil.WriteError(w, http.StatusBadRequest, "name and properties are required")
		return
	}

	s.logger.Info("editing dataset", "name", req.Name, "props", req.Properties)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.SetDatasetProperties(ctx, req.Name, req.Properties)
	if err != nil {
		s.logger.Error("edit dataset failed", "name", req.Name, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

type deleteDatasetRequest struct {
	Name      string `json:"name"`
	Confirm   string `json:"confirm"`
	Recursive bool   `json:"recursive"`
}

func (s *Server) handleDeleteDataset(w http.ResponseWriter, r *http.Request) {
	var req deleteDatasetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	// Safety: require typing the dataset name to confirm
	if req.Confirm != req.Name {
		addonutil.WriteError(w, http.StatusBadRequest, "type the full dataset name to confirm deletion")
		return
	}

	s.logger.Info("deleting dataset", "name", req.Name, "recursive", req.Recursive)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.DestroyDataset(ctx, req.Name, req.Recursive)
	if err != nil {
		s.logger.Error("delete dataset failed", "name", req.Name, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

// --- Snapshot Management ---

type createSnapshotRequest struct {
	Dataset   string `json:"dataset"`
	Name      string `json:"name,omitempty"`
	Recursive bool   `json:"recursive"`
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req createSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Dataset == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "dataset is required")
		return
	}

	snapName := req.Name
	if snapName == "" {
		snapName = "manual-" + time.Now().UTC().Format("2006-01-02-150405")
	}

	s.logger.Info("creating snapshot", "dataset", req.Dataset, "snap_name", snapName)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.CreateSnapshot(ctx, req.Dataset, snapName, req.Recursive)
	if err != nil {
		s.logger.Error("create snapshot failed", "dataset", req.Dataset, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

type deleteSnapshotRequest struct {
	Name    string `json:"name"`
	Confirm string `json:"confirm"`
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	var req deleteSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !strings.Contains(req.Name, "@") {
		addonutil.WriteError(w, http.StatusBadRequest, "name must be in dataset@snapshot format")
		return
	}
	if req.Confirm != req.Name {
		addonutil.WriteError(w, http.StatusBadRequest, "type the full snapshot name to confirm deletion")
		return
	}

	s.logger.Info("deleting snapshot", "name", req.Name)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.DestroySnapshot(ctx, req.Name)
	if err != nil {
		s.logger.Error("delete snapshot failed", "name", req.Name, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

type rollbackRequest struct {
	Name    string `json:"name"`
	Depth   string `json:"depth"`
	Confirm string `json:"confirm"`
}

func (s *Server) handleRollbackSnapshot(w http.ResponseWriter, r *http.Request) {
	var req rollbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !strings.Contains(req.Name, "@") {
		addonutil.WriteError(w, http.StatusBadRequest, "name must be in dataset@snapshot format")
		return
	}

	// Extract the dataset name for confirmation
	dataset := strings.SplitN(req.Name, "@", 2)[0]
	if req.Confirm != dataset {
		addonutil.WriteError(w, http.StatusBadRequest, "type the dataset name to confirm rollback")
		return
	}

	switch req.Depth {
	case "latest", "intermediate", "all":
		// valid
	case "":
		req.Depth = "latest"
	default:
		addonutil.WriteError(w, http.StatusBadRequest, "depth must be: latest, intermediate, or all")
		return
	}

	s.logger.Info("rolling back snapshot", "name", req.Name, "depth", req.Depth)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	result, err := s.engine.RollbackSnapshot(ctx, req.Name, req.Depth)
	if err != nil {
		s.logger.Error("rollback failed", "name", req.Name, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

// --- Scrub Controls ---

type scrubRequest struct {
	Pool string `json:"pool"`
}

func (s *Server) handleStartScrub(w http.ResponseWriter, r *http.Request) {
	var req scrubRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool is required")
		return
	}

	s.logger.Info("starting scrub", "pool", req.Pool)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.StartScrub(ctx, req.Pool)
	if err != nil {
		s.logger.Error("start scrub failed", "pool", req.Pool, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

func (s *Server) handlePauseScrub(w http.ResponseWriter, r *http.Request) {
	var req scrubRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool is required")
		return
	}

	s.logger.Info("pausing scrub", "pool", req.Pool)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.PauseScrub(ctx, req.Pool)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

func (s *Server) handleCancelScrub(w http.ResponseWriter, r *http.Request) {
	var req scrubRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool is required")
		return
	}

	s.logger.Info("canceling scrub", "pool", req.Pool)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.CancelScrub(ctx, req.Pool)
	if err != nil {
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

// --- Command Preview ---

type previewRequest struct {
	Action     string            `json:"action"`
	Pool       string            `json:"pool,omitempty"`
	Dataset    string            `json:"dataset,omitempty"`
	Parent     string            `json:"parent,omitempty"`
	Name       string            `json:"name,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
	Preset     string            `json:"preset,omitempty"`
	Quota      string            `json:"quota,omitempty"`
	Reserv     string            `json:"reservation,omitempty"`
	Recursive  bool              `json:"recursive,omitempty"`
	Depth      string            `json:"depth,omitempty"`
	ScrubOp    string            `json:"scrub_op,omitempty"`
	// Phase 4 fields
	OldDevice string   `json:"old_device,omitempty"`
	NewDevice string   `json:"new_device,omitempty"`
	Device    string   `json:"device,omitempty"`
	VdevType  string   `json:"vdev_type,omitempty"`
	Devices   []string `json:"devices,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	// Phase 5 fields
	Target     string `json:"target,omitempty"`
	DestTarget string `json:"dest_target,omitempty"`
	BaseSnap   string `json:"base_snap,omitempty"`

	// Remote replication preview fields (v0.5.1). When ReplicationMode is
	// "remote", the preview is built with BuildRemoteReplicationPipeline so the
	// user sees the ssh + optional pv stages. All fields follow the same
	// validation rules as the create-task handler.
	ReplicationMode string `json:"replication_mode,omitempty"`
	DestHost        string `json:"dest_host,omitempty"`
	DestPort        int    `json:"dest_port,omitempty"`
	DestUser        string `json:"dest_user,omitempty"`
	SSHKeyName      string `json:"ssh_key_name,omitempty"`
	BandwidthKbps   int    `json:"bandwidth_kbps,omitempty"`
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var (
		cmd      string
		warnings []string
	)
	switch req.Action {
	case "create_dataset":
		props := make(map[string]string)
		if preset, ok := DatasetPresets[req.Preset]; ok {
			props["recordsize"] = preset.RecordSize
			props["compression"] = preset.Compression
			props["atime"] = preset.Atime
			props["sync"] = preset.Sync
		}
		for k, v := range req.Properties {
			props[k] = v
		}
		if req.Quota != "" {
			props["quota"] = req.Quota
		}
		if req.Reserv != "" {
			props["reservation"] = req.Reserv
		}
		fullName := req.Parent + "/" + req.Name
		cmd = BuildCreateDatasetCommand(s.engine.zfsPath, fullName, props)

	case "destroy_dataset":
		cmd = BuildDestroyDatasetCommand(s.engine.zfsPath, req.Name, req.Recursive)

	case "create_snapshot":
		snapName := req.Name
		if snapName == "" {
			snapName = "manual-" + time.Now().UTC().Format("2006-01-02-150405")
		}
		cmd = BuildSnapshotCommand(s.engine.zfsPath, req.Dataset, snapName, req.Recursive)

	case "destroy_snapshot":
		cmd = s.engine.zfsPath + " destroy " + req.Name

	case "rollback":
		depth := req.Depth
		if depth == "" {
			depth = "latest"
		}
		cmd = BuildRollbackCommand(s.engine.zfsPath, req.Name, depth)

	case "scrub":
		op := req.ScrubOp
		if op == "" {
			op = "start"
		}
		cmd = BuildScrubCommand(s.engine.zpoolPath, req.Pool, op)

	case "set_properties":
		var parts []string
		for k, v := range req.Properties {
			parts = append(parts, fmt.Sprintf("%s set %s=%s %s", s.engine.zfsPath, k, v, req.Name))
		}
		cmd = strings.Join(parts, "\n")

	case "replace_device":
		cmd = BuildReplaceCommand(s.engine.zpoolPath, req.Pool, req.OldDevice, req.NewDevice)
		warnings = append(warnings, s.previewReplaceWarnings(r.Context(), req)...)

	case "add_vdev":
		cmd = BuildAddVdevCommand(s.engine.zpoolPath, req.Pool, req.VdevType, req.Devices)
		warnings = append(warnings, s.previewAddVdevWarnings(r.Context(), req)...)

	case "offline_device":
		cmd = BuildOfflineCommand(s.engine.zpoolPath, req.Pool, req.Device)

	case "online_device":
		cmd = BuildOnlineCommand(s.engine.zpoolPath, req.Pool, req.Device)

	case "clear_errors":
		cmd = BuildClearCommand(s.engine.zpoolPath, req.Pool, req.Device)

	case "identify_device":
		ledctlPath := s.engine.ledctlPath
		if ledctlPath == "" {
			ledctlPath = "ledctl"
		}
		cmd = BuildIdentifyCommand(ledctlPath, req.Device, req.Mode)
		if s.engine.ledctlPath == "" {
			warnings = append(warnings, "ledctl was not found on this host — drive-bay LED identification is unavailable. Install ledmon (the ledctl package) on the agent host to enable this action.")
		}

	case "replication":
		src := req.Dataset
		if src == "" {
			src = req.Target
		}
		snap := src + "@" + req.Name
		if req.Name == "" {
			snap = src + "@" + "repl-" + time.Now().UTC().Format("2006-01-02-150405")
		}
		if req.ReplicationMode == "remote" {
			// Validate the inputs the remote preview needs. sshKeyNameValid is
			// the same check the create-task handler runs so the preview can't
			// accept a name the actual run would later reject.
			if req.DestHost == "" || req.DestUser == "" || req.SSHKeyName == "" || req.DestTarget == "" {
				addonutil.WriteError(w, http.StatusBadRequest,
					"remote preview requires dest_host, dest_user, ssh_key_name, and dest_target")
				return
			}
			if !sshKeyNameValid(req.SSHKeyName) {
				addonutil.WriteError(w, http.StatusBadRequest,
					"ssh_key_name must contain only letters, digits, '-', '_' (max 64 chars)")
				return
			}
			port := req.DestPort
			if port == 0 {
				port = 22
			}
			tgt, terr := s.engine.RemoteTargetFromTask(req.DestTarget, req.DestHost, req.DestUser, port, req.SSHKeyName, req.BandwidthKbps)
			if terr != nil {
				addonutil.WriteError(w, http.StatusBadRequest, terr.Error())
				return
			}
			sshPath := s.engine.sshPath
			if sshPath == "" {
				sshPath = "ssh"
				warnings = append(warnings, "ssh was not found on this host — remote replication is unavailable. Install openssh-client on the agent.")
			}
			cmd = BuildRemoteReplicationPipeline(s.engine.zfsPath, sshPath, snap, req.BaseSnap, tgt)
			if req.BandwidthKbps > 0 && s.engine.pvPath == "" {
				warnings = append(warnings, "pv was not found on this host — the bandwidth cap will be silently dropped at runtime. Install pv on the agent.")
			}
		} else {
			cmd = BuildReplicationPipelineCommand(s.engine.zfsPath, snap, req.BaseSnap, req.DestTarget)
			if src != "" && src == req.DestTarget {
				warnings = append(warnings, "Source and destination datasets are the same — replication would overwrite itself.")
			}
		}

	default:
		addonutil.WriteError(w, http.StatusBadRequest, "unknown action: "+req.Action)
		return
	}

	resp := previewResponse{Command: cmd, Warnings: warnings}
	addonutil.WriteJSON(w, http.StatusOK, resp)
}

// previewResponse is the JSON shape returned by /api/preview. Warnings is
// omitted when empty to keep the wire format identical to previous versions
// for callers that don't care about warnings.
type previewResponse struct {
	Command  string   `json:"command"`
	Warnings []string `json:"warnings,omitempty"`
}

// previewReplaceWarnings collects non-fatal warnings for a replace_device
// preview: mainly the "replacement drive is smaller than the original" check.
// Warnings are best-effort — any lookup failure is logged and skipped.
func (s *Server) previewReplaceWarnings(ctx context.Context, req previewRequest) []string {
	if req.OldDevice == "" || req.NewDevice == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	oldSize := s.engine.DeviceSize(ctx, req.OldDevice)
	newSize := s.engine.DeviceSize(ctx, req.NewDevice)
	return CheckReplaceSize(oldSize, newSize, req.OldDevice, req.NewDevice)
}

// previewAddVdevWarnings collects non-fatal warnings for an add_vdev preview:
// mainly the "proposed vdev type doesn't match existing pool topology" check.
func (s *Server) previewAddVdevWarnings(ctx context.Context, req previewRequest) []string {
	if req.Pool == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pools, err := s.engine.ListPools(ctx)
	if err != nil {
		s.logger.Debug("preview add_vdev: list pools failed", "error", err)
		return nil
	}
	for _, p := range pools {
		if p.Name == req.Pool {
			return CheckVdevTypeMatch(p.Vdevs, req.VdevType)
		}
	}
	return nil
}

// --- Phase 4: Disk & Pool Operations ---

func (s *Server) handleListDisks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	disks, err := s.engine.ListAvailableDisks(ctx)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, "failed to list disks: "+err.Error())
		return
	}
	if disks == nil {
		disks = []AvailableDisk{}
	}
	addonutil.WriteJSON(w, http.StatusOK, disks)
}

type replaceDeviceRequest struct {
	Pool      string `json:"pool"`
	OldDevice string `json:"old_device"`
	NewDevice string `json:"new_device"`
	Confirm   string `json:"confirm"`
}

func (s *Server) handleReplaceDevice(w http.ResponseWriter, r *http.Request) {
	var req replaceDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" || req.OldDevice == "" || req.NewDevice == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool, old_device, and new_device are required")
		return
	}
	if req.Confirm != req.Pool {
		addonutil.WriteError(w, http.StatusBadRequest, "type the pool name to confirm replacement")
		return
	}

	s.logger.Info("replacing device", "pool", req.Pool, "old", req.OldDevice, "new", req.NewDevice)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	result, err := s.engine.ReplaceDevice(ctx, req.Pool, req.OldDevice, req.NewDevice)
	if err != nil {
		s.logger.Error("replace device failed", "pool", req.Pool, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	if s.collector != nil {
		now := time.Now().UTC()
		s.collector.EmitEvent(AgentEvent{
			ID:        fmt.Sprintf("replace-%s-%d", req.Pool, now.UnixNano()),
			Type:      "drive_replacement_started",
			Severity:  "warning",
			Message:   fmt.Sprintf("Drive replacement started: %s → %s on pool %s", req.OldDevice, req.NewDevice, req.Pool),
			Timestamp: now.Format(time.RFC3339),
		})
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

type addVdevRequest struct {
	Pool    string   `json:"pool"`
	Type    string   `json:"vdev_type"`
	Devices []string `json:"devices"`
	Confirm string   `json:"confirm"`
}

func (s *Server) handleAddVdev(w http.ResponseWriter, r *http.Request) {
	var req addVdevRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" || len(req.Devices) == 0 {
		addonutil.WriteError(w, http.StatusBadRequest, "pool and devices are required")
		return
	}
	if req.Confirm != req.Pool {
		addonutil.WriteError(w, http.StatusBadRequest, "type the pool name to confirm expansion")
		return
	}

	s.logger.Info("adding vdev", "pool", req.Pool, "type", req.Type, "devices", req.Devices)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	result, err := s.engine.AddVdev(ctx, req.Pool, req.Type, req.Devices)
	if err != nil {
		s.logger.Error("add vdev failed", "pool", req.Pool, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

type deviceRequest struct {
	Pool   string `json:"pool"`
	Device string `json:"device"`
}

func (s *Server) handleOfflineDevice(w http.ResponseWriter, r *http.Request) {
	var req deviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" || req.Device == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool and device are required")
		return
	}

	s.logger.Info("offlining device", "pool", req.Pool, "device", req.Device)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.OfflineDevice(ctx, req.Pool, req.Device)
	if err != nil {
		s.logger.Error("offline device failed", "pool", req.Pool, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

func (s *Server) handleOnlineDevice(w http.ResponseWriter, r *http.Request) {
	var req deviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" || req.Device == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool and device are required")
		return
	}

	s.logger.Info("onlining device", "pool", req.Pool, "device", req.Device)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.OnlineDevice(ctx, req.Pool, req.Device)
	if err != nil {
		s.logger.Error("online device failed", "pool", req.Pool, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

type identifyDeviceRequest struct {
	Device string `json:"device"`
	Mode   string `json:"mode,omitempty"` // "locate" (default) or "off"
}

func (s *Server) handleIdentifyDevice(w http.ResponseWriter, r *http.Request) {
	var req identifyDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Device == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "device is required")
		return
	}
	if req.Mode == "" {
		req.Mode = "locate"
	}
	if req.Mode != "locate" && req.Mode != "off" {
		addonutil.WriteError(w, http.StatusBadRequest, "mode must be 'locate' or 'off'")
		return
	}

	s.logger.Info("identifying device", "device", req.Device, "mode", req.Mode)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	result, err := s.engine.IdentifyDevice(ctx, req.Device, req.Mode)
	if err != nil {
		if errors.Is(err, ErrCapabilityUnavailable) {
			addonutil.WriteError(w, http.StatusNotImplemented, "ledctl is not installed on this agent host — drive-bay LED identification is unavailable")
			return
		}
		s.logger.Error("identify device failed", "device", req.Device, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	addonutil.WriteJSON(w, http.StatusOK, result)
}

type clearErrorsRequest struct {
	Pool   string `json:"pool"`
	Device string `json:"device,omitempty"`
}

func (s *Server) handleClearErrors(w http.ResponseWriter, r *http.Request) {
	var req clearErrorsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool is required")
		return
	}

	s.logger.Info("clearing errors", "pool", req.Pool, "device", req.Device)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.ClearErrors(ctx, req.Pool, req.Device)
	if err != nil {
		s.logger.Error("clear errors failed", "pool", req.Pool, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

// --- Phase 3: Scheduled Tasks ---

type taskView struct {
	agentdb.ScheduledTask
	NextRun string `json:"next_run,omitempty"`
}

func (s *Server) handleListTasks(w http.ResponseWriter, _ *http.Request) {
	tasks, err := agentdb.ListTasks(s.db)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []agentdb.ScheduledTask{}
	}

	// Enrich with next run times from scheduler. Replication tasks live on the
	// dedicated Replication page and are intentionally excluded here so they
	// cannot be edited through the generic /api/tasks handler (which would
	// clobber replication-specific fields).
	nextRuns := s.scheduler.NextRunTimes()
	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		if t.TaskType == "replication" {
			continue
		}
		v := taskView{ScheduledTask: t}
		if next, ok := nextRuns[t.ID]; ok {
			v.NextRun = next.UTC().Format(time.RFC3339)
		}
		views = append(views, v)
	}
	addonutil.WriteJSON(w, http.StatusOK, views)
}

type createTaskRequest struct {
	TaskType  string `json:"task_type"`
	Target    string `json:"target"`
	Schedule  string `json:"schedule"`
	Recursive bool   `json:"recursive"`
	Prefix    string `json:"prefix"`
	Retention int    `json:"retention"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.TaskType != "snapshot" && req.TaskType != "scrub" {
		addonutil.WriteError(w, http.StatusBadRequest, "task_type must be 'snapshot' or 'scrub'")
		return
	}
	if req.Target == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "target is required")
		return
	}
	if req.Schedule == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "schedule is required")
		return
	}
	if req.Prefix == "" {
		req.Prefix = "auto"
	}

	task := agentdb.ScheduledTask{
		TaskType:  req.TaskType,
		Target:    req.Target,
		Schedule:  req.Schedule,
		Recursive: req.Recursive,
		Enabled:   true,
		Prefix:    req.Prefix,
		Retention: req.Retention,
	}

	id, err := agentdb.InsertTask(s.db, task)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("created scheduled task", "task_id", id, "type", req.TaskType, "target", req.Target)

	// Reload scheduler to pick up the new task
	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.logger.Error("scheduler reload failed", "error", err)
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]any{"status": "created", "id": id})
}

type updateTaskRequest struct {
	Target    string `json:"target"`
	Schedule  string `json:"schedule"`
	Recursive bool   `json:"recursive"`
	Enabled   bool   `json:"enabled"`
	Prefix    string `json:"prefix"`
	Retention int    `json:"retention"`
}

func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
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
	if existing == nil {
		addonutil.WriteError(w, http.StatusNotFound, "task not found")
		return
	}
	if existing.TaskType == "replication" {
		addonutil.WriteError(w, http.StatusBadRequest, "use /api/replication/tasks/{id} to update replication tasks")
		return
	}

	var req updateTaskRequest
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

	if err := agentdb.UpdateTask(s.db, *existing); err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("updated scheduled task", "task_id", id)

	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.logger.Error("scheduler reload failed", "error", err)
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid task id")
		return
	}

	if err := agentdb.DeleteTask(s.db, id); err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("deleted scheduled task", "task_id", id)

	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.logger.Error("scheduler reload failed", "error", err)
	}

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleTaskHistory(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleJobHistory(w http.ResponseWriter, _ *http.Request) {
	jobs, err := agentdb.RecentJobs(s.db, 100)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []agentdb.JobRecord{}
	}
	addonutil.WriteJSON(w, http.StatusOK, jobs)
}

// --- Phase 3: Retention Stats ---

// RetentionDataset summarizes snapshot retention for a single dataset.
type RetentionDataset struct {
	Dataset        string `json:"dataset"`
	SnapshotCount  int    `json:"snapshot_count"`
	TotalUsed      uint64 `json:"total_used"`
	OldestSnapshot string `json:"oldest_snapshot,omitempty"`
	NewestSnapshot string `json:"newest_snapshot,omitempty"`
}

func (s *Server) handleRetentionStats(w http.ResponseWriter, _ *http.Request) {
	snapshots := s.collector.GetSnapshots()
	if snapshots == nil {
		addonutil.WriteJSON(w, http.StatusOK, []RetentionDataset{})
		return
	}

	// Group by dataset
	byDataset := make(map[string]*RetentionDataset)
	for _, snap := range snapshots {
		ds, ok := byDataset[snap.Dataset]
		if !ok {
			ds = &RetentionDataset{Dataset: snap.Dataset}
			byDataset[snap.Dataset] = ds
		}
		ds.SnapshotCount++
		ds.TotalUsed += snap.Used
		if ds.OldestSnapshot == "" || snap.Creation < ds.OldestSnapshot {
			ds.OldestSnapshot = snap.Creation
		}
		if ds.NewestSnapshot == "" || snap.Creation > ds.NewestSnapshot {
			ds.NewestSnapshot = snap.Creation
		}
	}

	result := make([]RetentionDataset, 0, len(byDataset))
	for _, ds := range byDataset {
		result = append(result, *ds)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Dataset < result[j].Dataset
	})

	addonutil.WriteJSON(w, http.StatusOK, result)
}

type retentionCleanupRequest struct {
	Dataset    string `json:"dataset"`
	OlderThan int    `json:"older_than_days"`
	Confirm    string `json:"confirm"`
}

func (s *Server) handleRetentionCleanup(w http.ResponseWriter, r *http.Request) {
	var req retentionCleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Dataset == "" || req.OlderThan <= 0 {
		addonutil.WriteError(w, http.StatusBadRequest, "dataset and older_than_days are required")
		return
	}
	if req.Confirm != req.Dataset {
		addonutil.WriteError(w, http.StatusBadRequest, "type the dataset name to confirm cleanup")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	snapshots, err := s.engine.ListSnapshots(ctx)
	if err != nil {
		addonutil.WriteError(w, http.StatusInternalServerError, "failed to list snapshots: "+err.Error())
		return
	}

	cutoff := time.Now().UTC().Add(-time.Duration(req.OlderThan) * 24 * time.Hour)
	var deleted int
	for _, snap := range snapshots {
		if snap.Dataset != req.Dataset {
			continue
		}
		// Parse creation timestamp (epoch seconds from -Hp output)
		epoch, err := strconv.ParseInt(snap.Creation, 10, 64)
		if err != nil {
			continue
		}
		created := time.Unix(epoch, 0)
		if created.Before(cutoff) {
			if _, err := s.engine.DestroySnapshot(ctx, snap.FullName); err != nil {
				s.logger.Error("retention cleanup: failed to delete", "snapshot", snap.FullName, "error", err)
				continue
			}
			deleted++
		}
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, map[string]any{"status": "completed", "deleted": deleted})
}

// refreshAndFlush refreshes telemetry cache and signals the hub forwarder.
func (s *Server) refreshAndFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.collector.Refresh(ctx)
	s.collector.RequestFlush()
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.cfg.Listen.Port)
	s.server = &http.Server{
		Addr:         addr,
		Handler:      s.mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("agent listen on %s: %w", addr, err)
	}

	s.logger.Info("agent server started", "addr", addr)
	return s.server.Serve(ln)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("agent server shutting down")
	return s.server.Shutdown(ctx)
}
