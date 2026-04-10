package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	agentdb "github.com/pineappledr/vigil-addons/zfs-manager/internal/db"
	_ "modernc.org/sqlite"
)

// mockScheduler satisfies SchedulerReloader for testing.
type mockScheduler struct {
	reloadCalled int
	nextRuns     map[int64]time.Time
}

func (m *mockScheduler) Reload(_ context.Context) error {
	m.reloadCalled++
	return nil
}

func (m *mockScheduler) NextRunTimes() map[int64]time.Time {
	if m.nextRuns == nil {
		return map[int64]time.Time{}
	}
	return m.nextRuns
}

func testSetup(t *testing.T) (*Server, *mockScheduler, *sql.DB) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS scheduled_tasks (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			task_type        TEXT    NOT NULL,
			target           TEXT    NOT NULL,
			schedule         TEXT    NOT NULL,
			recursive        BOOLEAN NOT NULL DEFAULT 0,
			enabled          BOOLEAN NOT NULL DEFAULT 1,
			prefix           TEXT    NOT NULL DEFAULT 'auto',
			retention        INTEGER NOT NULL DEFAULT 0,
			created_at       DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at       DATETIME NOT NULL DEFAULT (datetime('now')),
			dest_target      TEXT,
			replication_mode TEXT,
			last_sent_snap   TEXT
		);
		CREATE TABLE IF NOT EXISTS job_history (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id     INTEGER,
			job_type    TEXT    NOT NULL,
			trigger     TEXT    NOT NULL,
			started_at  DATETIME NOT NULL,
			finished_at DATETIME,
			status      TEXT    NOT NULL DEFAULT 'running',
			message     TEXT,
			FOREIGN KEY (task_id) REFERENCES scheduled_tasks(id) ON DELETE SET NULL
		);
	`)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	sched := &mockScheduler{}

	// Engine and Collector can be nil for task CRUD handlers (they don't touch ZFS)
	srv := &Server{
		engine:    nil,
		collector: nil,
		db:        db,
		scheduler: sched,
		mux:       http.NewServeMux(),
		logger:    logger,
	}
	srv.routes()

	return srv, sched, db
}

func doRequest(t *testing.T, mux http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rr.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rr.Body.String())
	}
}

// --- Task CRUD ---

func TestCreateTask(t *testing.T) {
	srv, sched, _ := testSetup(t)

	body := map[string]any{
		"task_type": "snapshot",
		"target":    "tank/data",
		"schedule":  "0 * * * *",
		"recursive": true,
		"prefix":    "hourly",
		"retention": 24,
	}

	rr := doRequest(t, srv.mux, "POST", "/api/tasks", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["status"] != "created" {
		t.Errorf("status = %v, want created", resp["status"])
	}
	if resp["id"] == nil || resp["id"].(float64) < 1 {
		t.Errorf("expected positive id, got %v", resp["id"])
	}
	if sched.reloadCalled != 1 {
		t.Errorf("scheduler.Reload called %d times, want 1", sched.reloadCalled)
	}
}

func TestCreateTask_InvalidType(t *testing.T) {
	srv, _, _ := testSetup(t)

	body := map[string]any{
		"task_type": "invalid",
		"target":    "tank/data",
		"schedule":  "0 * * * *",
	}

	rr := doRequest(t, srv.mux, "POST", "/api/tasks", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestCreateTask_MissingTarget(t *testing.T) {
	srv, _, _ := testSetup(t)

	body := map[string]any{
		"task_type": "snapshot",
		"schedule":  "0 * * * *",
	}

	rr := doRequest(t, srv.mux, "POST", "/api/tasks", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestCreateTask_DefaultPrefix(t *testing.T) {
	srv, _, db := testSetup(t)

	body := map[string]any{
		"task_type": "snapshot",
		"target":    "tank/data",
		"schedule":  "0 * * * *",
	}

	rr := doRequest(t, srv.mux, "POST", "/api/tasks", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	tasks, _ := agentdb.ListTasks(db)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Prefix != "auto" {
		t.Errorf("prefix = %q, want %q", tasks[0].Prefix, "auto")
	}
}

func TestListTasks(t *testing.T) {
	srv, sched, db := testSetup(t)

	id1, _ := agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "hourly", Retention: 24,
	})
	agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "scrub", Target: "tank", Schedule: "0 2 * * 0",
		Enabled: true, Prefix: "auto",
	})

	nextTime := time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)
	sched.nextRuns = map[int64]time.Time{id1: nextTime}

	rr := doRequest(t, srv.mux, "GET", "/api/tasks", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var tasks []taskView
	decodeJSON(t, rr, &tasks)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	// First task should have NextRun enriched
	if tasks[0].NextRun == "" {
		t.Error("expected next_run to be set for task 1")
	}
	// Second task has no next run in mock
	if tasks[1].NextRun != "" {
		t.Errorf("expected empty next_run for task 2, got %q", tasks[1].NextRun)
	}
}

func TestUpdateTask(t *testing.T) {
	srv, sched, db := testSetup(t)

	id, _ := agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "hourly", Retention: 24,
	})

	body := map[string]any{
		"target":    "tank/backup",
		"schedule":  "0 0 * * *",
		"recursive": false,
		"enabled":   false,
		"prefix":    "nightly",
		"retention": 7,
	}

	rr := doRequest(t, srv.mux, "PUT", "/api/tasks/"+itoa(id), body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	got, _ := agentdb.GetTask(db, id)
	if got.Target != "tank/backup" {
		t.Errorf("target = %q, want %q", got.Target, "tank/backup")
	}
	if got.Enabled {
		t.Error("expected enabled = false")
	}
	if got.Retention != 7 {
		t.Errorf("retention = %d, want %d", got.Retention, 7)
	}
	if sched.reloadCalled != 1 {
		t.Errorf("scheduler.Reload called %d times, want 1", sched.reloadCalled)
	}
}

func TestUpdateTask_NotFound(t *testing.T) {
	srv, _, _ := testSetup(t)

	body := map[string]any{
		"target":   "tank/data",
		"schedule": "0 * * * *",
		"enabled":  true,
		"prefix":   "auto",
	}

	rr := doRequest(t, srv.mux, "PUT", "/api/tasks/999", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestDeleteTask(t *testing.T) {
	srv, sched, db := testSetup(t)

	id, _ := agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "auto",
	})

	rr := doRequest(t, srv.mux, "DELETE", "/api/tasks/"+itoa(id), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	got, _ := agentdb.GetTask(db, id)
	if got != nil {
		t.Fatal("expected task to be deleted")
	}
	if sched.reloadCalled != 1 {
		t.Errorf("scheduler.Reload called %d times, want 1", sched.reloadCalled)
	}
}

func TestDeleteTask_InvalidID(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "DELETE", "/api/tasks/abc", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

// --- Job History ---

func TestJobHistory(t *testing.T) {
	srv, _, db := testSetup(t)

	agentdb.InsertJob(db, nil, "snapshot", "manual")
	agentdb.InsertJob(db, nil, "scrub", "scheduled")

	rr := doRequest(t, srv.mux, "GET", "/api/jobs", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var jobs []agentdb.JobRecord
	decodeJSON(t, rr, &jobs)
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
}

func TestTaskHistory(t *testing.T) {
	srv, _, db := testSetup(t)

	taskID, _ := agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "auto",
	})
	agentdb.InsertJob(db, &taskID, "snapshot", "scheduled")
	agentdb.InsertJob(db, &taskID, "snapshot", "scheduled")
	// Job for a different task
	agentdb.InsertJob(db, nil, "scrub", "manual")

	rr := doRequest(t, srv.mux, "GET", "/api/tasks/"+itoa(taskID)+"/history", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var jobs []agentdb.JobRecord
	decodeJSON(t, rr, &jobs)
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs for this task, got %d", len(jobs))
	}
}

func TestTaskHistory_InvalidID(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "GET", "/api/tasks/xyz/history", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

// --- Create Scrub Task ---

func TestCreateScrubTask(t *testing.T) {
	srv, sched, db := testSetup(t)

	body := map[string]any{
		"task_type": "scrub",
		"target":    "tank",
		"schedule":  "0 2 * * 0",
	}

	rr := doRequest(t, srv.mux, "POST", "/api/tasks", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	tasks, _ := agentdb.ListTasks(db)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].TaskType != "scrub" {
		t.Errorf("task_type = %q, want %q", tasks[0].TaskType, "scrub")
	}
	if sched.reloadCalled != 1 {
		t.Errorf("scheduler.Reload called %d times, want 1", sched.reloadCalled)
	}
}

// --- Health ---

func TestHealth(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "GET", "/health", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var resp map[string]string
	decodeJSON(t, rr, &resp)
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

// --- Phase 4: Disk & Pool Operations ---

func TestReplaceDevice_MissingFields(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/pool/replace", map[string]any{
		"pool": "tank",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestReplaceDevice_ConfirmRequired(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/pool/replace", map[string]any{
		"pool":       "tank",
		"old_device": "sda",
		"new_device": "/dev/sdb",
		"confirm":    "wrong",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAddVdev_MissingFields(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/pool/add-vdev", map[string]any{
		"pool": "tank",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAddVdev_ConfirmRequired(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/pool/add-vdev", map[string]any{
		"pool":      "tank",
		"vdev_type": "mirror",
		"devices":   []string{"/dev/sda", "/dev/sdb"},
		"confirm":   "wrong",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestOfflineDevice_MissingFields(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/devices/offline", map[string]any{
		"pool": "tank",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestOnlineDevice_MissingFields(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/devices/online", map[string]any{
		"device": "sda",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestClearErrors_MissingPool(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/pool/clear", map[string]any{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestIdentifyDevice_MissingDevice(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/devices/identify", map[string]any{
		"mode": "locate",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestIdentifyDevice_InvalidMode(t *testing.T) {
	srv, _, _ := testSetup(t)

	rr := doRequest(t, srv.mux, "POST", "/api/devices/identify", map[string]any{
		"device": "sda",
		"mode":   "blink",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
