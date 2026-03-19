package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// JobRecord represents a row in the job_history table.
type JobRecord struct {
	ID         int64      `json:"id"`
	JobType    string     `json:"job_type"`
	Trigger    string     `json:"trigger"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Status     string     `json:"status"`
	OutputLog  string     `json:"output_log,omitempty"`
}

const timeFmt = "2006-01-02 15:04:05"

// InsertJob creates a new job_history record with status "running" and returns its ID.
func InsertJob(db *sql.DB, jobType, trigger string) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO job_history (job_type, trigger, started_at, status) VALUES (?, ?, ?, 'running')`,
		jobType, trigger, time.Now().UTC().Format(timeFmt),
	)
	if err != nil {
		return 0, fmt.Errorf("insert job: %w", err)
	}
	return res.LastInsertId()
}

// CompleteJob updates a job record with its final state.
func CompleteJob(db *sql.DB, id int64, exitCode int, status, outputLog string) error {
	_, err := db.Exec(
		`UPDATE job_history SET finished_at = ?, exit_code = ?, status = ?, output_log = ? WHERE id = ?`,
		time.Now().UTC().Format(timeFmt), exitCode, status, outputLog, id,
	)
	if err != nil {
		return fmt.Errorf("complete job %d: %w", id, err)
	}
	return nil
}

// RecentJobs returns the most recent N job records ordered by start time descending.
func RecentJobs(db *sql.DB, limit int) ([]JobRecord, error) {
	rows, err := db.Query(
		`SELECT id, job_type, trigger, started_at, finished_at, exit_code, status, COALESCE(output_log, '') FROM job_history ORDER BY started_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query recent jobs: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// RecentJobsSince returns job records started after the given cutoff time.
func RecentJobsSince(db *sql.DB, since time.Time, limit int) ([]JobRecord, error) {
	rows, err := db.Query(
		`SELECT id, job_type, trigger, started_at, finished_at, exit_code, status, COALESCE(output_log, '') FROM job_history WHERE started_at >= ? ORDER BY started_at DESC LIMIT ?`,
		since.UTC().Format(timeFmt), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query jobs since: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// scanJobs reads JobRecord rows from a query result.
func scanJobs(rows *sql.Rows) ([]JobRecord, error) {
	var jobs []JobRecord
	for rows.Next() {
		var j JobRecord
		var startStr string
		var finishStr sql.NullString
		var exitCode sql.NullInt64

		if err := rows.Scan(&j.ID, &j.JobType, &j.Trigger, &startStr, &finishStr, &exitCode, &j.Status, &j.OutputLog); err != nil {
			return nil, fmt.Errorf("scan job row: %w", err)
		}

		j.StartedAt = parseFlexTime(startStr)
		if finishStr.Valid {
			t := parseFlexTime(finishStr.String)
			if !t.IsZero() {
				j.FinishedAt = &t
			}
		}
		if exitCode.Valid {
			code := int(exitCode.Int64)
			j.ExitCode = &code
		}

		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// parseFlexTime tries multiple formats to handle driver-level DATETIME variations.
func parseFlexTime(s string) time.Time {
	for _, fmt := range []string{
		timeFmt,
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05+00:00",
	} {
		if t, err := time.Parse(fmt, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// PruneOlderThan deletes job records older than the given duration.
func PruneOlderThan(db *sql.DB, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(timeFmt)
	res, err := db.Exec(`DELETE FROM job_history WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune jobs: %w", err)
	}
	return res.RowsAffected()
}

// PruneTelemetryQueue deletes queued telemetry records older than the given duration.
func PruneTelemetryQueue(db *sql.DB, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(timeFmt)
	res, err := db.Exec(`DELETE FROM telemetry_queue WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune telemetry queue: %w", err)
	}
	return res.RowsAffected()
}

// StartPruneLoop runs a daily background loop that deletes job_history and
// telemetry_queue records older than the given retention period.
// It stops when ctx is cancelled.
func StartPruneLoop(ctx context.Context, database *sql.DB, retention time.Duration, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		prune := func() {
			if n, err := PruneOlderThan(database, retention); err != nil {
				logger.Error("job history prune failed", "error", err)
			} else if n > 0 {
				logger.Info("pruned old job history records", "deleted", n)
			}
			if n, err := PruneTelemetryQueue(database, retention); err != nil {
				logger.Error("telemetry queue prune failed", "error", err)
			} else if n > 0 {
				logger.Info("pruned old telemetry queue records", "deleted", n)
			}
		}

		prune() // Run once immediately on startup

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
