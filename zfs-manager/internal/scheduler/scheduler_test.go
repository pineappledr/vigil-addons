package scheduler

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pineappledr/vigil-addons/zfs-manager/internal/agent"
	agentdb "github.com/pineappledr/vigil-addons/zfs-manager/internal/db"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	// Run the schema migration
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
			last_sent_snap   TEXT,
			dest_host        TEXT,
			dest_port        INTEGER,
			dest_user         TEXT,
			ssh_key_name     TEXT,
			bandwidth_kbps   INTEGER,
			manage_remote_retention BOOLEAN NOT NULL DEFAULT 0,
			use_bookmarks           BOOLEAN NOT NULL DEFAULT 0
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
	return db
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestStartEmpty(t *testing.T) {
	db := openTestDB(t)
	logger := testLogger()

	// Engine/Collector are nil — Start with no tasks should succeed without calling them
	s := New(nil, nil, db, logger)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start with empty tasks: %v", err)
	}
	defer s.Stop()

	if len(s.NextRunTimes()) != 0 {
		t.Errorf("expected 0 next run times, got %d", len(s.NextRunTimes()))
	}
}

func TestStartRegistersEnabledTasks(t *testing.T) {
	db := openTestDB(t)
	logger := testLogger()

	agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "hourly",
	})
	agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "scrub", Target: "tank", Schedule: "0 2 * * 0",
		Enabled: false, Prefix: "auto",
	})

	s := New(nil, nil, db, logger)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	nextRuns := s.NextRunTimes()
	// Only 1 enabled task should be registered
	if len(nextRuns) != 1 {
		t.Fatalf("expected 1 next run time, got %d", len(nextRuns))
	}

	// Verify the next run is in the future
	for _, next := range nextRuns {
		if !next.After(time.Now()) {
			t.Errorf("next run %v should be in the future", next)
		}
	}
}

func TestReload(t *testing.T) {
	db := openTestDB(t)
	logger := testLogger()

	s := New(nil, nil, db, logger)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	if len(s.NextRunTimes()) != 0 {
		t.Fatal("expected 0 tasks initially")
	}

	// Add a task and reload
	agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "*/5 * * * *",
		Enabled: true, Prefix: "auto",
	})

	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(s.NextRunTimes()) != 1 {
		t.Fatalf("expected 1 task after reload, got %d", len(s.NextRunTimes()))
	}
}

func TestInvalidCronExpression(t *testing.T) {
	db := openTestDB(t)
	logger := testLogger()

	agentdb.InsertTask(db, agentdb.ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "not a cron",
		Enabled: true, Prefix: "auto",
	})

	s := New(nil, nil, db, logger)
	// Start should not fail even if a task has a bad expression; it logs the error
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start should not fail on bad cron: %v", err)
	}
	defer s.Stop()

	if len(s.NextRunTimes()) != 0 {
		t.Errorf("bad cron should not be registered, got %d entries", len(s.NextRunTimes()))
	}
}

// newSchedulerWithCollector wires a Scheduler to a real (but unstarted)
// Collector so event-emission assertions can inspect Collector.LastEvent.
// Engine is nil — these tests only exercise paths that don't touch it.
func newSchedulerWithCollector(t *testing.T) (*Scheduler, *agent.Collector) {
	t.Helper()
	db := openTestDB(t)
	coll := agent.NewCollector("agent-test", "test-host", nil, testLogger())
	return New(nil, coll, db, testLogger()), coll
}

func TestEmitFailureEvent_Snapshot(t *testing.T) {
	s, coll := newSchedulerWithCollector(t)
	s.emitFailureEvent(agentdb.ScheduledTask{ID: 7, TaskType: "snapshot", Target: "tank/data"},
		errNewf("zfs snapshot: permission denied"))
	evt := coll.LastEvent()
	if evt == nil {
		t.Fatal("expected LastEvent to be set")
	}
	if evt.Type != "snapshot_task_failed" {
		t.Errorf("type = %q, want snapshot_task_failed", evt.Type)
	}
	if evt.Severity != "critical" {
		t.Errorf("severity = %q, want critical", evt.Severity)
	}
	if !strings.Contains(evt.Message, "tank/data") || !strings.Contains(evt.Message, "permission denied") {
		t.Errorf("message should mention dataset and underlying error, got %q", evt.Message)
	}
}

func TestEmitFailureEvent_Scrub(t *testing.T) {
	s, coll := newSchedulerWithCollector(t)
	s.emitFailureEvent(agentdb.ScheduledTask{ID: 8, TaskType: "scrub", Target: "tank"},
		errNewf("pool is suspended"))
	evt := coll.LastEvent()
	if evt == nil || evt.Type != "scrub_task_failed" {
		t.Fatalf("want scrub_task_failed, got %+v", evt)
	}
}

func TestEmitFailureEvent_ReplicationCarriesDest(t *testing.T) {
	s, coll := newSchedulerWithCollector(t)
	dest := "backup/tank"
	s.emitFailureEvent(agentdb.ScheduledTask{
		ID: 9, TaskType: "replication", Target: "tank/data", DestTarget: &dest,
	}, errNewf("ssh: connection refused"))
	evt := coll.LastEvent()
	if evt == nil || evt.Type != "replication_failed" {
		t.Fatalf("want replication_failed, got %+v", evt)
	}
	if !strings.Contains(evt.Message, "tank/data") || !strings.Contains(evt.Message, "backup/tank") {
		t.Errorf("replication failure should name source and dest, got %q", evt.Message)
	}
}

func TestEmitFailureEvent_UnknownTaskTypeEmitsNothing(t *testing.T) {
	s, coll := newSchedulerWithCollector(t)
	s.emitFailureEvent(agentdb.ScheduledTask{ID: 1, TaskType: "mystery", Target: "tank"},
		errNewf("unknown"))
	if evt := coll.LastEvent(); evt != nil {
		t.Errorf("expected no event for unknown task type, got %+v", evt)
	}
}

func TestEmitEvent_NilCollectorDoesNotPanic(t *testing.T) {
	// Tests that construct Scheduler with collector=nil must still be able
	// to traverse executeTask without a nil dereference.
	s := New(nil, nil, openTestDB(t), testLogger())
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emitEvent on nil collector panicked: %v", r)
		}
	}()
	s.emitEvent(agent.AgentEvent{Type: "snapshot_task_succeeded"})
	s.requestFlush()
}

// errNewf is a tiny helper so tests stay readable without importing errors.
func errNewf(msg string) error { return &schedErr{msg} }

type schedErr struct{ msg string }

func (e *schedErr) Error() string { return e.msg }
