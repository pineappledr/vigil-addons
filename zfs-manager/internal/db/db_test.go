package db

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// --- Scheduled Tasks ---

func TestInsertAndGetTask(t *testing.T) {
	db := openTestDB(t)

	task := ScheduledTask{
		TaskType:  "snapshot",
		Target:    "tank/data",
		Schedule:  "0 * * * *",
		Recursive: true,
		Enabled:   true,
		Prefix:    "hourly",
		Retention: 24,
	}

	id, err := InsertTask(db, task)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if id < 1 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, err := GetTask(db, id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got == nil {
		t.Fatal("task not found")
	}
	if got.TaskType != "snapshot" {
		t.Errorf("task_type = %q, want %q", got.TaskType, "snapshot")
	}
	if got.Target != "tank/data" {
		t.Errorf("target = %q, want %q", got.Target, "tank/data")
	}
	if got.Schedule != "0 * * * *" {
		t.Errorf("schedule = %q, want %q", got.Schedule, "0 * * * *")
	}
	if !got.Recursive {
		t.Error("expected recursive = true")
	}
	if !got.Enabled {
		t.Error("expected enabled = true")
	}
	if got.Prefix != "hourly" {
		t.Errorf("prefix = %q, want %q", got.Prefix, "hourly")
	}
	if got.Retention != 24 {
		t.Errorf("retention = %d, want %d", got.Retention, 24)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	db := openTestDB(t)

	got, err := GetTask(db, 999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent task")
	}
}

func TestUpdateTask(t *testing.T) {
	db := openTestDB(t)

	id, _ := InsertTask(db, ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "auto",
	})

	task, _ := GetTask(db, id)
	task.Target = "tank/backup"
	task.Schedule = "0 0 * * *"
	task.Enabled = false
	task.Retention = 7

	if err := UpdateTask(db, *task); err != nil {
		t.Fatalf("update task: %v", err)
	}

	got, _ := GetTask(db, id)
	if got.Target != "tank/backup" {
		t.Errorf("target = %q, want %q", got.Target, "tank/backup")
	}
	if got.Schedule != "0 0 * * *" {
		t.Errorf("schedule = %q, want %q", got.Schedule, "0 0 * * *")
	}
	if got.Enabled {
		t.Error("expected enabled = false")
	}
	if got.Retention != 7 {
		t.Errorf("retention = %d, want %d", got.Retention, 7)
	}
}

func TestDeleteTask(t *testing.T) {
	db := openTestDB(t)

	id, _ := InsertTask(db, ScheduledTask{
		TaskType: "scrub", Target: "tank", Schedule: "0 2 * * 0",
		Enabled: true, Prefix: "auto",
	})

	if err := DeleteTask(db, id); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	got, _ := GetTask(db, id)
	if got != nil {
		t.Fatal("expected task to be deleted")
	}
}

func TestListTasks(t *testing.T) {
	db := openTestDB(t)

	InsertTask(db, ScheduledTask{TaskType: "snapshot", Target: "tank/a", Schedule: "0 * * * *", Enabled: true, Prefix: "auto"})
	InsertTask(db, ScheduledTask{TaskType: "scrub", Target: "tank", Schedule: "0 2 * * 0", Enabled: false, Prefix: "auto"})

	tasks, err := ListTasks(db)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestListEnabledTasks(t *testing.T) {
	db := openTestDB(t)

	InsertTask(db, ScheduledTask{TaskType: "snapshot", Target: "tank/a", Schedule: "0 * * * *", Enabled: true, Prefix: "auto"})
	InsertTask(db, ScheduledTask{TaskType: "scrub", Target: "tank", Schedule: "0 2 * * 0", Enabled: false, Prefix: "auto"})

	tasks, err := ListEnabledTasks(db)
	if err != nil {
		t.Fatalf("list enabled tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 enabled task, got %d", len(tasks))
	}
	if tasks[0].Target != "tank/a" {
		t.Errorf("expected target tank/a, got %s", tasks[0].Target)
	}
}

// --- Job History ---

func TestInsertAndCompleteJob(t *testing.T) {
	db := openTestDB(t)

	taskID, _ := InsertTask(db, ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "auto",
	})
	jobID, err := InsertJob(db, &taskID, "snapshot", "scheduled")
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if jobID < 1 {
		t.Fatalf("expected positive job id, got %d", jobID)
	}

	if err := CompleteJob(db, jobID, "success", ""); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	jobs, err := RecentJobs(db, 10)
	if err != nil {
		t.Fatalf("recent jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Status != "success" {
		t.Errorf("status = %q, want %q", jobs[0].Status, "success")
	}
	if jobs[0].FinishedAt == "" {
		t.Error("expected finished_at to be set")
	}
}

func TestInsertJob_NilTaskID(t *testing.T) {
	db := openTestDB(t)

	jobID, err := InsertJob(db, nil, "snapshot", "manual")
	if err != nil {
		t.Fatalf("insert job with nil task_id: %v", err)
	}

	jobs, _ := RecentJobs(db, 10)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].TaskID != nil {
		t.Errorf("expected nil task_id, got %v", jobs[0].TaskID)
	}
	_ = jobID
}

func TestJobsForTask(t *testing.T) {
	db := openTestDB(t)

	// Insert a task so FK is valid
	taskID, _ := InsertTask(db, ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "auto",
	})

	otherTaskID := int64(999)
	InsertJob(db, &taskID, "snapshot", "scheduled")
	InsertJob(db, &taskID, "snapshot", "scheduled")
	InsertJob(db, &otherTaskID, "scrub", "scheduled")

	jobs, err := JobsForTask(db, taskID, 50)
	if err != nil {
		t.Fatalf("jobs for task: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs for task %d, got %d", taskID, len(jobs))
	}
}

func TestRecentJobs_Limit(t *testing.T) {
	db := openTestDB(t)

	for i := 0; i < 5; i++ {
		InsertJob(db, nil, "snapshot", "scheduled")
	}

	jobs, err := RecentJobs(db, 3)
	if err != nil {
		t.Fatalf("recent jobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs (limited), got %d", len(jobs))
	}
}

func TestCompleteJob_Error(t *testing.T) {
	db := openTestDB(t)

	jobID, _ := InsertJob(db, nil, "snapshot", "scheduled")
	CompleteJob(db, jobID, "error", "zfs command failed: dataset not found")

	jobs, _ := RecentJobs(db, 10)
	if jobs[0].Status != "error" {
		t.Errorf("status = %q, want %q", jobs[0].Status, "error")
	}
	if jobs[0].Message != "zfs command failed: dataset not found" {
		t.Errorf("message = %q, want error message", jobs[0].Message)
	}
}

func TestPruneJobs(t *testing.T) {
	db := openTestDB(t)

	// Insert a job with a very old started_at
	oldTime := time.Now().UTC().Add(-60 * 24 * time.Hour).Format(timeFmt)
	db.Exec(
		`INSERT INTO job_history (task_id, job_type, trigger, started_at, finished_at, status) VALUES (NULL, 'snapshot', 'scheduled', ?, ?, 'success')`,
		oldTime, oldTime,
	)
	// Insert a recent job
	InsertJob(db, nil, "snapshot", "scheduled")

	deleted, err := PruneJobs(db, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("prune jobs: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted, got %d", deleted)
	}

	jobs, _ := RecentJobs(db, 10)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 remaining job, got %d", len(jobs))
	}
}

func TestDeleteTask_CascadesJobHistory(t *testing.T) {
	db := openTestDB(t)

	taskID, _ := InsertTask(db, ScheduledTask{
		TaskType: "snapshot", Target: "tank/data", Schedule: "0 * * * *",
		Enabled: true, Prefix: "auto",
	})
	InsertJob(db, &taskID, "snapshot", "scheduled")

	DeleteTask(db, taskID)

	// Jobs should still exist but with NULL task_id (ON DELETE SET NULL)
	jobs, _ := RecentJobs(db, 10)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job after task delete, got %d", len(jobs))
	}
	if jobs[0].TaskID != nil {
		t.Errorf("expected nil task_id after cascade, got %v", jobs[0].TaskID)
	}
}
