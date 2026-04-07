package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const timeFmt = "2006-01-02 15:04:05"

// Open initializes the SQLite database and runs migrations.
func Open(dbPath string) (*sql.DB, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("create db directory %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if err := os.Chmod(dbPath, 0600); err != nil && !os.IsNotExist(err) {
		db.Close()
		return nil, fmt.Errorf("chmod db file: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return db, nil
}

func migrate(db *sql.DB) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		task_type   TEXT    NOT NULL,  -- 'snapshot' or 'scrub'
		target      TEXT    NOT NULL,  -- dataset or pool name
		schedule    TEXT    NOT NULL,  -- cron expression (5-field)
		recursive   BOOLEAN NOT NULL DEFAULT 0,
		enabled     BOOLEAN NOT NULL DEFAULT 1,
		prefix      TEXT    NOT NULL DEFAULT 'auto',
		retention   INTEGER NOT NULL DEFAULT 0,  -- max snapshots to keep (0 = unlimited)
		created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at  DATETIME NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS job_history (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id     INTEGER,
		job_type    TEXT    NOT NULL,
		trigger     TEXT    NOT NULL,  -- 'scheduled' or 'manual'
		started_at  DATETIME NOT NULL,
		finished_at DATETIME,
		status      TEXT    NOT NULL DEFAULT 'running',
		message     TEXT,
		FOREIGN KEY (task_id) REFERENCES scheduled_tasks(id) ON DELETE SET NULL
	);
	`
	_, err := db.Exec(schema)
	return err
}

// --- Scheduled Tasks ---

// ScheduledTask represents a periodic snapshot or scrub task.
type ScheduledTask struct {
	ID        int64  `json:"id"`
	TaskType  string `json:"task_type"`
	Target    string `json:"target"`
	Schedule  string `json:"schedule"`
	Recursive bool   `json:"recursive"`
	Enabled   bool   `json:"enabled"`
	Prefix    string `json:"prefix"`
	Retention int    `json:"retention"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// InsertTask creates a new scheduled task.
func InsertTask(db *sql.DB, t ScheduledTask) (int64, error) {
	now := time.Now().UTC().Format(timeFmt)
	res, err := db.Exec(
		`INSERT INTO scheduled_tasks (task_type, target, schedule, recursive, enabled, prefix, retention, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TaskType, t.Target, t.Schedule, t.Recursive, t.Enabled, t.Prefix, t.Retention, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert task: %w", err)
	}
	return res.LastInsertId()
}

// UpdateTask modifies an existing scheduled task.
func UpdateTask(db *sql.DB, t ScheduledTask) error {
	now := time.Now().UTC().Format(timeFmt)
	_, err := db.Exec(
		`UPDATE scheduled_tasks SET target = ?, schedule = ?, recursive = ?, enabled = ?, prefix = ?, retention = ?, updated_at = ? WHERE id = ?`,
		t.Target, t.Schedule, t.Recursive, t.Enabled, t.Prefix, t.Retention, now, t.ID,
	)
	if err != nil {
		return fmt.Errorf("update task %d: %w", t.ID, err)
	}
	return nil
}

// DeleteTask removes a scheduled task.
func DeleteTask(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM scheduled_tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete task %d: %w", id, err)
	}
	return nil
}

// GetTask returns a single task by ID.
func GetTask(db *sql.DB, id int64) (*ScheduledTask, error) {
	row := db.QueryRow(
		`SELECT id, task_type, target, schedule, recursive, enabled, prefix, retention, created_at, updated_at FROM scheduled_tasks WHERE id = ?`, id,
	)
	return scanTask(row)
}

// ListTasks returns all scheduled tasks.
func ListTasks(db *sql.DB) ([]ScheduledTask, error) {
	rows, err := db.Query(
		`SELECT id, task_type, target, schedule, recursive, enabled, prefix, retention, created_at, updated_at FROM scheduled_tasks ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	var tasks []ScheduledTask
	for rows.Next() {
		var t ScheduledTask
		if err := rows.Scan(&t.ID, &t.TaskType, &t.Target, &t.Schedule, &t.Recursive, &t.Enabled, &t.Prefix, &t.Retention, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// ListEnabledTasks returns all enabled scheduled tasks.
func ListEnabledTasks(db *sql.DB) ([]ScheduledTask, error) {
	rows, err := db.Query(
		`SELECT id, task_type, target, schedule, recursive, enabled, prefix, retention, created_at, updated_at FROM scheduled_tasks WHERE enabled = 1 ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query enabled tasks: %w", err)
	}
	defer rows.Close()

	var tasks []ScheduledTask
	for rows.Next() {
		var t ScheduledTask
		if err := rows.Scan(&t.ID, &t.TaskType, &t.Target, &t.Schedule, &t.Recursive, &t.Enabled, &t.Prefix, &t.Retention, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func scanTask(row *sql.Row) (*ScheduledTask, error) {
	var t ScheduledTask
	if err := row.Scan(&t.ID, &t.TaskType, &t.Target, &t.Schedule, &t.Recursive, &t.Enabled, &t.Prefix, &t.Retention, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	return &t, nil
}

// --- Job History ---

// JobRecord represents a row in the job_history table.
type JobRecord struct {
	ID         int64  `json:"id"`
	TaskID     *int64 `json:"task_id,omitempty"`
	JobType    string `json:"job_type"`
	Trigger    string `json:"trigger"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// InsertJob creates a new running job record.
func InsertJob(db *sql.DB, taskID *int64, jobType, trigger string) (int64, error) {
	now := time.Now().UTC().Format(timeFmt)
	res, err := db.Exec(
		`INSERT INTO job_history (task_id, job_type, trigger, started_at, status) VALUES (?, ?, ?, ?, 'running')`,
		taskID, jobType, trigger, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert job: %w", err)
	}
	return res.LastInsertId()
}

// CompleteJob finalizes a job record.
func CompleteJob(db *sql.DB, id int64, status, message string) error {
	now := time.Now().UTC().Format(timeFmt)
	_, err := db.Exec(
		`UPDATE job_history SET finished_at = ?, status = ?, message = ? WHERE id = ?`,
		now, status, message, id,
	)
	if err != nil {
		return fmt.Errorf("complete job %d: %w", id, err)
	}
	return nil
}

// RecentJobs returns job history ordered by start time descending.
func RecentJobs(db *sql.DB, limit int) ([]JobRecord, error) {
	rows, err := db.Query(
		`SELECT id, task_id, job_type, trigger, started_at, COALESCE(finished_at, ''), status, COALESCE(message, '')
		 FROM job_history ORDER BY started_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query recent jobs: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// JobsForTask returns job history for a specific task.
func JobsForTask(db *sql.DB, taskID int64, limit int) ([]JobRecord, error) {
	rows, err := db.Query(
		`SELECT id, task_id, job_type, trigger, started_at, COALESCE(finished_at, ''), status, COALESCE(message, '')
		 FROM job_history WHERE task_id = ? ORDER BY started_at DESC LIMIT ?`, taskID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query jobs for task %d: %w", taskID, err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

func scanJobs(rows *sql.Rows) ([]JobRecord, error) {
	var jobs []JobRecord
	for rows.Next() {
		var j JobRecord
		if err := rows.Scan(&j.ID, &j.TaskID, &j.JobType, &j.Trigger, &j.StartedAt, &j.FinishedAt, &j.Status, &j.Message); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// PruneJobs deletes job records older than the given retention.
func PruneJobs(db *sql.DB, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(timeFmt)
	res, err := db.Exec(`DELETE FROM job_history WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune jobs: %w", err)
	}
	return res.RowsAffected()
}

// StartPruneLoop runs a daily background prune of old job records.
func StartPruneLoop(ctx context.Context, database *sql.DB, retention time.Duration, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		prune := func() {
			if n, err := PruneJobs(database, retention); err != nil {
				logger.Error("job history prune failed", "error", err)
			} else if n > 0 {
				logger.Info("pruned old job history records", "deleted", n)
			}
		}
		prune()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
}
