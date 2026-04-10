package scheduler

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"

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
