package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	agentdb "github.com/pineappledr/vigil-addons/snapraid/internal/db"
	"github.com/pineappledr/vigil-addons/snapraid/internal/engine"
)

// Pipeline orchestrates SnapRAID operations and records results in the job history.
type Pipeline struct {
	engine *engine.Engine
	cfg    *config.AgentConfig
	db     *sql.DB
	logger *slog.Logger
}

// RunMaintenance executes the full automated maintenance pipeline:
// touch -> diff -> safety gates (SMART + diff thresholds) -> sync -> scrub.
func (p *Pipeline) RunMaintenance(ctx context.Context) {
	p.logger.Info("maintenance pipeline started")

	// Step 1: touch
	if !p.runStep(ctx, "touch", "scheduled", func(ctx context.Context) (int, string, error) {
		report, err := p.engine.Touch(ctx)
		if err != nil {
			return 0, "", err
		}
		return report.ExitCode, report.Output, nil
	}) {
		return
	}

	// Step 2: diff
	var diffReport *engine.DiffReport
	if !p.runStep(ctx, "diff", "pre-flight", func(ctx context.Context) (int, string, error) {
		var err error
		diffReport, err = p.engine.Diff(ctx)
		if err != nil {
			return 0, "", err
		}
		return 0, formatDiffSummary(diffReport), nil
	}) {
		return
	}

	// Step 3: SMART gate
	var smartReport *engine.SmartReport
	if !p.runStep(ctx, "smart", "pre-flight", func(ctx context.Context) (int, string, error) {
		var err error
		smartReport, err = p.engine.Smart(ctx)
		if err != nil {
			return 0, "", err
		}
		return 0, "smart check completed", nil
	}) {
		return
	}

	// Evaluate Gate 1: SMART
	gate := CheckSMART(smartReport, p.cfg.Thresholds)
	if !gate.Passed {
		p.logger.Error("maintenance aborted: SMART gate failed", "reason", gate.Reason)
		p.recordGateFailure("smart_gate", gate.Reason)
		return
	}

	// Evaluate Gate 2: Diff thresholds
	gate = CheckDiffThresholds(diffReport, p.cfg.Thresholds)
	if !gate.Passed {
		p.logger.Error("maintenance aborted: diff threshold gate failed", "reason", gate.Reason)
		p.recordGateFailure("diff_gate", gate.Reason)
		return
	}

	p.logger.Info("all pre-flight gates passed")

	// Step 4: sync
	if !p.runStep(ctx, "sync", "scheduled", func(ctx context.Context) (int, string, error) {
		report, err := p.engine.Sync(ctx, engine.SyncOptions{
			PreHash: p.cfg.Sync.PreHash,
		}, nil)
		if err != nil {
			return 0, "", err
		}
		return report.ExitCode, report.Output, nil
	}) {
		return
	}

	// Step 5: scrub
	p.runStep(ctx, "scrub", "scheduled", func(ctx context.Context) (int, string, error) {
		report, err := p.engine.Scrub(ctx, engine.ScrubOptions{
			Plan:          p.cfg.Scrub.Plan,
			OlderThanDays: p.cfg.Scrub.OlderThanDays,
		})
		if err != nil {
			return 0, "", err
		}
		return report.ExitCode, report.Output, nil
	})

	p.logger.Info("maintenance pipeline completed")
}

// RunScrubOnly executes a standalone scrub job.
func (p *Pipeline) RunScrubOnly(ctx context.Context) {
	p.runStep(ctx, "scrub", "scheduled", func(ctx context.Context) (int, string, error) {
		report, err := p.engine.Scrub(ctx, engine.ScrubOptions{
			Plan:          p.cfg.Scrub.Plan,
			OlderThanDays: p.cfg.Scrub.OlderThanDays,
		})
		if err != nil {
			return 0, "", err
		}
		return report.ExitCode, report.Output, nil
	})
}

// RunSmartCheck executes a standalone SMART check.
func (p *Pipeline) RunSmartCheck(ctx context.Context) {
	p.runStep(ctx, "smart", "scheduled", func(ctx context.Context) (int, string, error) {
		report, err := p.engine.Smart(ctx)
		if err != nil {
			return 0, "", err
		}
		return 0, formatSmartSummary(report), nil
	})
}

// RunStatusRefresh executes a standalone status refresh.
func (p *Pipeline) RunStatusRefresh(ctx context.Context) {
	p.runStep(ctx, "status", "scheduled", func(ctx context.Context) (int, string, error) {
		report, err := p.engine.Status(ctx)
		if err != nil {
			return 0, "", err
		}
		return 0, formatStatusSummary(report), nil
	})
}

// stepFunc executes a SnapRAID operation and returns (exitCode, output, error).
type stepFunc func(ctx context.Context) (int, string, error)

// runStep executes a single pipeline step with job history recording.
// Returns true if the step succeeded (exit code 0 or 2), false otherwise.
func (p *Pipeline) runStep(ctx context.Context, jobType, trigger string, fn stepFunc) bool {
	p.logger.Info("pipeline step started", "job", jobType)

	jobID, dbErr := agentdb.InsertJob(p.db, jobType, trigger)
	if dbErr != nil {
		p.logger.Error("failed to record job start", "job", jobType, "error", dbErr)
	}

	exitCode, output, err := fn(ctx)

	status := "success"
	if err != nil {
		// Check Gate 3: concurrency lock
		gate := CheckLock(err)
		if !gate.Passed {
			p.logger.Warn("pipeline step blocked by lock", "job", jobType)
			status = "aborted"
		} else {
			status = "error"
		}
		p.logger.Error("pipeline step failed", "job", jobType, "error", err)
		if jobID > 0 {
			agentdb.CompleteJob(p.db, jobID, -1, status, err.Error())
		}
		return false
	}

	switch exitCode {
	case 0:
		status = "success"
	case 2:
		status = "warning"
	default:
		status = "error"
	}

	if jobID > 0 {
		agentdb.CompleteJob(p.db, jobID, exitCode, status, output)
	}

	p.logger.Info("pipeline step completed", "job", jobType, "exit_code", exitCode, "status", status)
	return status == "success" || status == "warning"
}

// recordGateFailure logs a gate failure as a job history entry.
func (p *Pipeline) recordGateFailure(gateType, reason string) {
	jobID, err := agentdb.InsertJob(p.db, gateType, "pre-flight")
	if err != nil {
		p.logger.Error("failed to record gate failure", "gate", gateType, "error", err)
		return
	}
	agentdb.CompleteJob(p.db, jobID, -1, "aborted", reason)
}

func formatDiffSummary(r *engine.DiffReport) string {
	return "added=" + itoa(r.Added) +
		" removed=" + itoa(r.Removed) +
		" updated=" + itoa(r.Updated) +
		" moved=" + itoa(r.Moved) +
		" copied=" + itoa(r.Copied) +
		" restored=" + itoa(r.Restored)
}

func formatSmartSummary(r *engine.SmartReport) string {
	return "disks=" + itoa(len(r.Disks)) +
		" overall_fail_probability=" + ftoa(r.OverallFailProbability) + "%"
}

func formatStatusSummary(r *engine.StatusReport) string {
	return "files=" + itoa(r.Files) +
		" unsynced=" + itoa(r.UnsyncedBlocks) +
		" bad_blocks=" + itoa(r.BadBlocks) +
		" unscrubbed=" + ftoa(r.UnscrubbedPercent) + "%"
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func ftoa(f float64) string {
	return fmt.Sprintf("%.1f", f)
}
