package jobs

import (
	"testing"
	"time"
)

func TestJobRecordToStatus(t *testing.T) {
	started := time.Now().UTC().Add(-90 * time.Second)

	t.Run("active job has elapsed and zero percent", func(t *testing.T) {
		rec := JobRecord{
			JobID:     "burnin-sda-1",
			Command:   "burnin",
			Status:    StatusRunning,
			Phase:     "WRITE_PATTERN",
			StartedAt: started,
		}
		got := jobRecordToStatus(rec, false)
		if got["status"] != "running" {
			t.Fatalf("status: want running, got %v", got["status"])
		}
		if got["phase"] != "WRITE_PATTERN" {
			t.Fatalf("phase: want WRITE_PATTERN, got %v", got["phase"])
		}
		elapsed, _ := got["elapsed_sec"].(int64)
		if elapsed < 85 || elapsed > 120 {
			t.Fatalf("elapsed_sec: want ~90, got %d", elapsed)
		}
		if got["progress_percent"] != 0 {
			t.Fatalf("progress_percent: running job should report 0, got %v", got["progress_percent"])
		}
		if _, ok := got["fail_reason"]; ok {
			t.Fatal("fail_reason should be omitted when empty")
		}
	})

	t.Run("finished job reports 100 and carries fail_reason", func(t *testing.T) {
		completed := started.Add(30 * time.Second)
		rec := JobRecord{
			JobID:       "burnin-sda-2",
			Command:     "burnin",
			Status:      StatusFailed,
			Phase:       PhaseComplete,
			StartedAt:   started,
			CompletedAt: &completed,
			FailReason:  "I/O error on block 4096",
		}
		got := jobRecordToStatus(rec, true)
		if got["status"] != "failed" {
			t.Fatalf("status: want failed, got %v", got["status"])
		}
		if got["progress_percent"] != 100 {
			t.Fatalf("progress_percent: finished job should report 100, got %v", got["progress_percent"])
		}
		if got["elapsed_sec"] != int64(30) {
			t.Fatalf("elapsed_sec: want 30, got %v", got["elapsed_sec"])
		}
		if got["fail_reason"] != "I/O error on block 4096" {
			t.Fatalf("fail_reason mismatch: %v", got["fail_reason"])
		}
	})

	t.Run("completed job reports 100", func(t *testing.T) {
		completed := started.Add(60 * time.Second)
		rec := JobRecord{
			JobID:       "burnin-sda-3",
			Command:     "preclear",
			Status:      StatusCompleted,
			Phase:       PhaseComplete,
			StartedAt:   started,
			CompletedAt: &completed,
		}
		got := jobRecordToStatus(rec, false)
		if got["progress_percent"] != 100 {
			t.Fatalf("completed job should report 100, got %v", got["progress_percent"])
		}
	})
}
