package db

import (
	"database/sql"
	"fmt"
	"time"
)

// JobRecord represents a row in the job_history table.
type JobRecord struct {
	ID         int64
	JobType    string
	Trigger    string
	StartedAt  time.Time
	FinishedAt *time.Time
	ExitCode   *int
	Status     string
	OutputLog  string
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

	var jobs []JobRecord
	for rows.Next() {
		var j JobRecord
		var startStr string
		var finishStr sql.NullString
		var exitCode sql.NullInt64

		if err := rows.Scan(&j.ID, &j.JobType, &j.Trigger, &startStr, &finishStr, &exitCode, &j.Status, &j.OutputLog); err != nil {
			return nil, fmt.Errorf("scan job row: %w", err)
		}

		j.StartedAt, _ = time.Parse(timeFmt, startStr)
		if finishStr.Valid {
			t, _ := time.Parse(timeFmt, finishStr.String)
			j.FinishedAt = &t
		}
		if exitCode.Valid {
			code := int(exitCode.Int64)
			j.ExitCode = &code
		}

		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
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
