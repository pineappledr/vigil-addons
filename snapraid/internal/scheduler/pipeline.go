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

// EventEmitter allows the pipeline to emit notification events upstream
// without importing the agent package directly.
type EventEmitter interface {
	EmitEvent(eventType, severity, message string)
}

// JobTracker allows the pipeline to report active job state upstream
// so the dashboard can display running operations.
type JobTracker interface {
	TrackJob(jobType, trigger, phase string)
	UpdateProgress(pct int)
	ClearJob()
}

// Pipeline orchestrates SnapRAID operations and records results in the job history.
type Pipeline struct {
	engine  *engine.Engine
	cfg     *config.AgentConfig
	db      *sql.DB
	emitter EventEmitter
	tracker JobTracker
	logger  *slog.Logger
}

// RunMaintenance executes the full automated maintenance pipeline:
// touch -> diff -> safety gates (SMART + diff thresholds) -> sync -> scrub.
func (p *Pipeline) RunMaintenance(ctx context.Context) {
	p.logger.Info("maintenance pipeline started")
	p.emit("maintenance_started", "info", "Maintenance pipeline started")

	// Gate 0: Validate content and parity files exist and are non-empty.
	gate := CheckConfigFiles(p.engine)
	if !gate.Passed {
		p.logger.Error("maintenance aborted: config files gate failed", "reason", gate.Reason)
		p.recordGateFailure("config_files_gate", gate.Reason)
		p.emit("gate_failed", "warning", "Maintenance aborted: "+gate.Reason)
		return
	}

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
		return 0, diffReport.Output, nil
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
	gate = CheckSMART(smartReport, p.cfg.Thresholds)
	if !gate.Passed {
		p.logger.Error("maintenance aborted: SMART gate failed", "reason", gate.Reason)
		p.recordGateFailure("smart_gate", gate.Reason)
		p.emit("gate_failed", "warning", "Maintenance aborted: "+gate.Reason)
		return
	}

	// Evaluate Gate 2: Diff thresholds
	gate = CheckDiffThresholds(diffReport, p.cfg.Thresholds)
	if !gate.Passed {
		p.logger.Error("maintenance aborted: diff threshold gate failed", "reason", gate.Reason)
		p.recordGateFailure("diff_gate", gate.Reason)
		p.emit("gate_failed", "warning", "Maintenance aborted: "+gate.Reason)
		return
	}

	p.logger.Info("all pre-flight gates passed")

	// Pre-sync hook
	if err := runHook(ctx, "pre_sync", p.cfg.Hooks.PreSync, p.logger); err != nil {
		p.logger.Error("maintenance aborted: pre-sync hook failed", "error", err)
		p.recordGateFailure("pre_sync_hook", err.Error())
		p.emit("gate_failed", "warning", "Pre-sync hook failed: "+err.Error())
		return
	}

	// Docker container management before sync
	restored := p.manageContainers(ctx, true)

	// Step 4: sync
	syncOk := p.runStep(ctx, "sync", "scheduled", func(ctx context.Context) (int, string, error) {
		progress, stopProgress := p.progressChan()
		defer stopProgress()
		report, err := p.engine.Sync(ctx, engine.SyncOptions{
			PreHash: p.cfg.Sync.PreHash,
		}, progress)
		if err != nil {
			return 0, "", err
		}
		return report.ExitCode, report.Output, nil
	})

	// Restore Docker containers after sync (regardless of sync outcome)
	if restored != nil {
		restored()
	}

	// Post-sync hook
	if hookErr := runHook(ctx, "post_sync", p.cfg.Hooks.PostSync, p.logger); hookErr != nil {
		p.logger.Warn("post-sync hook failed", "error", hookErr)
	}

	if !syncOk {
		return
	}

	// Step 5: scrub
	var scrubExitCode int
	p.runStep(ctx, "scrub", "scheduled", func(ctx context.Context) (int, string, error) {
		progress, stopProgress := p.progressChan()
		defer stopProgress()
		report, err := p.engine.Scrub(ctx, engine.ScrubOptions{
			Plan:          p.cfg.Scrub.Plan,
			OlderThanDays: p.cfg.Scrub.OlderThanDays,
		}, progress)
		if err != nil {
			return 0, "", err
		}
		scrubExitCode = report.ExitCode
		return report.ExitCode, report.Output, nil
	})

	// Step 6: auto-fix bad blocks if scrub reported errors (exit code 2)
	if p.cfg.Scrub.AutoFixBadBlocks && scrubExitCode == 2 {
		p.logger.Info("scrub reported bad blocks, running auto-fix")
		p.runStep(ctx, "fix", "auto-fix", func(ctx context.Context) (int, string, error) {
			progress, stopProgress := p.progressChan()
			defer stopProgress()
			report, err := p.engine.Fix(ctx, engine.FixOptions{BadBlocksOnly: true}, progress)
			if err != nil {
				return 0, "", err
			}
			return report.ExitCode, report.Output, nil
		})
	}

	p.logger.Info("maintenance pipeline completed")
	p.emit("maintenance_complete", "info", "Maintenance pipeline completed successfully")
}

// RunScrubOnly executes a standalone scrub job.
func (p *Pipeline) RunScrubOnly(ctx context.Context) {
	p.emit("scrub_started", "info", "Standalone scrub started")

	var scrubExitCode int
	p.runStep(ctx, "scrub", "scheduled", func(ctx context.Context) (int, string, error) {
		progress, stopProgress := p.progressChan()
		defer stopProgress()
		report, err := p.engine.Scrub(ctx, engine.ScrubOptions{
			Plan:          p.cfg.Scrub.Plan,
			OlderThanDays: p.cfg.Scrub.OlderThanDays,
		}, progress)
		if err != nil {
			return 0, "", err
		}
		scrubExitCode = report.ExitCode
		return report.ExitCode, report.Output, nil
	})

	if p.cfg.Scrub.AutoFixBadBlocks && scrubExitCode == 2 {
		p.logger.Info("scrub reported bad blocks, running auto-fix")
		p.runStep(ctx, "fix", "auto-fix", func(ctx context.Context) (int, string, error) {
			progress, stopProgress := p.progressChan()
			defer stopProgress()
			report, err := p.engine.Fix(ctx, engine.FixOptions{BadBlocksOnly: true}, progress)
			if err != nil {
				return 0, "", err
			}
			return report.ExitCode, report.Output, nil
		})
	}

	p.emit("scrub_complete", "info", "Standalone scrub completed")
}

// RunStatusRefresh executes a standalone status refresh.
func (p *Pipeline) RunStatusRefresh(ctx context.Context) {
	p.runStep(ctx, "status", "scheduled", func(ctx context.Context) (int, string, error) {
		report, err := p.engine.Status(ctx)
		if err != nil {
			return 0, "", err
		}
		return 0, report.Output, nil
	})

	p.emit("status_refresh_complete", "info", "Status refresh completed")
}

// progressChan creates a buffered progress channel that drains into the
// tracker's UpdateProgress. Returns the channel and a stop function.
// Returns (nil, noop) if no tracker is configured.
func (p *Pipeline) progressChan() (chan<- int, func()) {
	if p.tracker == nil {
		return nil, func() {}
	}
	ch := make(chan int, 4)
	done := make(chan struct{})
	go func() {
		for pct := range ch {
			p.tracker.UpdateProgress(pct)
		}
		close(done)
	}()
	return ch, func() { close(ch); <-done }
}

// stepFunc executes a SnapRAID operation and returns (exitCode, output, error).
type stepFunc func(ctx context.Context) (int, string, error)

// runStep executes a single pipeline step with job history recording.
// Returns true if the step succeeded (exit code 0 or 2), false otherwise.
func (p *Pipeline) runStep(ctx context.Context, jobType, trigger string, fn stepFunc) bool {
	p.logger.Info("pipeline step started", "job", jobType)

	if p.tracker != nil {
		p.tracker.TrackJob(jobType, trigger, "running")
		defer p.tracker.ClearJob()
	}

	jobID, dbErr := agentdb.InsertJob(p.db, jobType, trigger)
	if dbErr != nil {
		p.logger.Error("failed to record job start", "job", jobType, "error", dbErr)
	}

	exitCode, output, err := fn(ctx)

	// Signal completion so non-streaming jobs (diff, smart, status, touch)
	// don't leave the progress bar stuck at 0% for their entire duration.
	// For streaming jobs (sync, scrub, fix) this is redundant but harmless —
	// the progress channel has already been fully drained before fn returns.
	if err == nil && p.tracker != nil {
		p.tracker.UpdateProgress(100)
	}

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
		p.logger.Error("pipeline step failed", "job", jobType, "error", err, "output", output)
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

	// Log the full command output so it appears in container logs.
	if output != "" {
		p.logger.Info("snapraid output", "job", jobType, "output", output)
	}

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

// emit sends a notification event upstream if an emitter is configured.
func (p *Pipeline) emit(eventType, severity, message string) {
	if p.emitter != nil {
		p.emitter.EmitEvent(eventType, severity, message)
	}
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
