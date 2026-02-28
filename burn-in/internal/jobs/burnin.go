package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/drive"
)

// Phase constants for the burn-in pipeline.
const (
	PhasePreflight     = "PRE-FLIGHT"
	PhaseSmartShort    = "SMART_SHORT"
	PhaseBadblocks     = "BADBLOCKS"
	PhaseSmartExtended = "SMART_EXTENDED"
	PhaseComplete      = "COMPLETE"
)

// Severity levels for telemetry log frames.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// BurninParams holds the configuration for a single burn-in job.
type BurninParams struct {
	BlockSize        int  `json:"block_size"`
	ConcurrentBlocks int  `json:"concurrent_blocks"`
	AbortOnError     bool `json:"abort_on_error"`
	LogDir           string `json:"log_dir"`
}

// BurninResult holds the final outcome of a burn-in job.
type BurninResult struct {
	Passed        bool               `json:"passed"`
	FailReason    string             `json:"fail_reason,omitempty"`
	DriveInfo     *drive.DriveInfo   `json:"drive_info"`
	Baseline      *drive.SmartSnapshot `json:"baseline_snapshot"`
	FinalSnapshot *drive.SmartSnapshot `json:"final_snapshot"`
	FinalDelta    *drive.SmartDelta    `json:"final_delta"`
	BadblockErrs  int                `json:"badblock_errors"`
	ShortTest     *drive.TestResult  `json:"short_test"`
	LongTest      *drive.TestResult  `json:"long_test"`
	TotalDuration time.Duration      `json:"total_duration"`
}

// TelemetrySink is the interface for emitting telemetry frames during a job.
// This decouples the orchestrator from the concrete HubTelemetry client.
type TelemetrySink interface {
	SendProgress(jobID, command, phase, phaseDetail string, percent, speedMbps float64, tempC int, elapsedSec, etaSec int64, badblockErrs int, smartDeltas json.RawMessage) error
	SendLog(jobID, severity, message string) error
}

// RunBurnin executes the full five-phase burn-in pipeline for a single drive.
// It blocks until completion or context cancellation.
func RunBurnin(ctx context.Context, jobID, devicePath string, params BurninParams, sink TelemetrySink, logger *slog.Logger) (*BurninResult, error) {
	start := time.Now()
	result := &BurninResult{Passed: true}

	emit := &emitter{
		sink:    sink,
		jobID:   jobID,
		command: "burnin",
		start:   start,
		logger:  logger,
	}

	// ── Phase 1: PRE-FLIGHT ─────────────────────────────────────────────
	emit.phase(PhasePreflight, "resolving device", 0)

	driveInfo, err := drive.ResolveDrive(devicePath)
	if err != nil {
		return nil, emit.fail(result, PhasePreflight, "device resolution failed: %v", err)
	}
	result.DriveInfo = driveInfo

	emit.log(SeverityInfo, "device resolved: %s model=%s serial=%s", driveInfo.Path, driveInfo.Model, driveInfo.Serial)
	emit.phase(PhasePreflight, "safety check", 5)

	if err := drive.IsSafeTarget(driveInfo.Path); err != nil {
		return nil, emit.fail(result, PhasePreflight, "safety check failed: %v", err)
	}

	emit.phase(PhasePreflight, "baseline SMART snapshot", 10)

	baseline, err := drive.TakeSnapshot(driveInfo.Path)
	if err != nil {
		return nil, emit.fail(result, PhasePreflight, "baseline snapshot failed: %v", err)
	}
	result.Baseline = baseline

	emit.log(SeverityInfo, "baseline SMART snapshot captured")
	emit.phaseComplete(PhasePreflight)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 2: SMART SHORT ────────────────────────────────────────────
	emit.phase(PhaseSmartShort, "starting short test", 15)

	shortResult, err := drive.RunShortTest(ctx, driveInfo.Path)
	if err != nil {
		return nil, emit.fail(result, PhaseSmartShort, "short test failed: %v", err)
	}
	result.ShortTest = shortResult

	if !shortResult.Passed {
		emit.log(SeverityWarning, "SMART short test reported failure: %s", shortResult.Status)
	}

	emit.phase(PhaseSmartShort, "post-short snapshot", 20)

	postShort, err := drive.TakeSnapshot(driveInfo.Path)
	if err != nil {
		emit.log(SeverityWarning, "post-short snapshot failed: %v", err)
	} else {
		shortDelta := drive.ComputeDelta(baseline, postShort)
		if shortDelta.Degraded {
			emit.log(SeverityWarning, "SMART degradation detected after short test")
			emit.smartWarning(shortDelta)
		}
	}

	emit.phaseComplete(PhaseSmartShort)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 3: BADBLOCKS ──────────────────────────────────────────────
	emit.phase(PhaseBadblocks, "preparing", 25)

	if params.LogDir != "" {
		if err := drive.EnsureLogDir(params.LogDir); err != nil {
			return nil, emit.fail(result, PhaseBadblocks, "cannot create log dir: %v", err)
		}
	}

	bbResult, err := drive.RunBadblocks(ctx, driveInfo.Path, params.BlockSize, params.ConcurrentBlocks, params.AbortOnError, params.LogDir, func(progress drive.BadblocksProgress) {
		// Map badblocks progress (0-100%) into the overall 25-75% band.
		overallPercent := 25.0 + progress.Percent*0.5
		emit.progress(PhaseBadblocks, progress.Phase, overallPercent, 0, 0, progress.Errors, nil)
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, emit.fail(result, PhaseBadblocks, "badblocks failed: %v", err)
	}

	result.BadblockErrs = bbResult.Errors

	if bbResult.Errors > 0 {
		emit.log(SeverityError, "badblocks found %d error(s)", bbResult.Errors)
		if params.AbortOnError {
			result.Passed = false
			result.FailReason = fmt.Sprintf("badblocks found %d error(s)", bbResult.Errors)
			emit.phaseComplete(PhaseBadblocks)
			return result, emit.fail(result, PhaseBadblocks, "%s", result.FailReason)
		}
	} else {
		emit.log(SeverityInfo, "badblocks completed with zero errors")
	}

	emit.phaseComplete(PhaseBadblocks)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 4: SMART EXTENDED ─────────────────────────────────────────
	emit.phase(PhaseSmartExtended, "starting extended test", 75)

	longResult, err := drive.RunLongTest(ctx, driveInfo.Path)
	if err != nil {
		return nil, emit.fail(result, PhaseSmartExtended, "extended test failed: %v", err)
	}
	result.LongTest = longResult

	if !longResult.Passed {
		emit.log(SeverityWarning, "SMART extended test reported failure: %s", longResult.Status)
	}

	emit.phase(PhaseSmartExtended, "final snapshot", 90)

	finalSnap, err := drive.TakeSnapshot(driveInfo.Path)
	if err != nil {
		emit.log(SeverityWarning, "final SMART snapshot failed: %v", err)
	} else {
		result.FinalSnapshot = finalSnap
		result.FinalDelta = drive.ComputeDelta(baseline, finalSnap)

		if result.FinalDelta.Degraded {
			emit.log(SeverityWarning, "SMART degradation detected in final snapshot")
			emit.smartWarning(result.FinalDelta)
		}
	}

	emit.phaseComplete(PhaseSmartExtended)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 5: COMPLETE ───────────────────────────────────────────────
	result.TotalDuration = time.Since(start)

	// Determine pass/fail.
	if !shortResult.Passed || (longResult != nil && !longResult.Passed) {
		result.Passed = false
		result.FailReason = "SMART self-test failure"
	}
	if result.FinalDelta != nil && result.FinalDelta.Degraded {
		result.Passed = false
		if result.FailReason != "" {
			result.FailReason += "; "
		}
		result.FailReason += "SMART attribute degradation"
	}
	if bbResult.Errors > 0 {
		result.Passed = false
		if result.FailReason != "" {
			result.FailReason += "; "
		}
		result.FailReason += fmt.Sprintf("%d bad block(s)", bbResult.Errors)
	}

	if result.Passed {
		emit.log(SeverityInfo, "burn-in PASSED for %s (%s) in %s", driveInfo.Path, driveInfo.Serial, result.TotalDuration.Round(time.Second))
		emit.jobComplete(result)
	} else {
		emit.log(SeverityError, "burn-in FAILED for %s (%s): %s", driveInfo.Path, driveInfo.Serial, result.FailReason)
		emit.jobFailed(result)
	}

	emit.phaseComplete(PhaseComplete)

	return result, nil
}

// emitter wraps TelemetrySink with convenience methods for the orchestrator.
type emitter struct {
	sink    TelemetrySink
	jobID   string
	command string
	start   time.Time
	logger  *slog.Logger
}

func (e *emitter) elapsed() int64 {
	return int64(time.Since(e.start).Seconds())
}

func (e *emitter) phase(phase, detail string, percent float64) {
	e.progress(phase, detail, percent, 0, 0, 0, nil)
}

func (e *emitter) progress(phase, detail string, percent, speedMbps float64, tempC int, badblockErrs int, smartDeltas json.RawMessage) {
	if e.sink == nil {
		return
	}
	if err := e.sink.SendProgress(e.jobID, e.command, phase, detail, percent, speedMbps, tempC, e.elapsed(), 0, badblockErrs, smartDeltas); err != nil {
		e.logger.Warn("failed to transmit progress", "error", err, "phase", phase)
	}
}

func (e *emitter) log(severity, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e.logger.Info(msg, "job_id", e.jobID, "severity", severity)
	if e.sink == nil {
		return
	}
	if err := e.sink.SendLog(e.jobID, severity, msg); err != nil {
		e.logger.Warn("failed to transmit log", "error", err)
	}
}

func (e *emitter) phaseComplete(phase string) {
	e.log(SeverityInfo, "phase %s complete", phase)
}

func (e *emitter) smartWarning(delta *drive.SmartDelta) {
	data, err := json.Marshal(delta.Deltas)
	if err != nil {
		return
	}
	e.progress("SMART_WARNING", "attribute degradation detected", 0, 0, 0, 0, data)
}

func (e *emitter) jobComplete(result *BurninResult) {
	var deltaJSON json.RawMessage
	if result.FinalDelta != nil {
		deltaJSON, _ = json.Marshal(result.FinalDelta.Deltas)
	}
	e.progress(PhaseComplete, "burn-in passed", 100, 0, 0, result.BadblockErrs, deltaJSON)
}

func (e *emitter) jobFailed(result *BurninResult) {
	var deltaJSON json.RawMessage
	if result.FinalDelta != nil {
		deltaJSON, _ = json.Marshal(result.FinalDelta.Deltas)
	}
	e.progress(PhaseComplete, result.FailReason, 100, 0, 0, result.BadblockErrs, deltaJSON)
}

func (e *emitter) fail(result *BurninResult, phase, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	result.Passed = false
	result.FailReason = msg
	result.TotalDuration = time.Since(e.start)
	e.log(SeverityError, "%s", msg)
	e.jobFailed(result)
	return fmt.Errorf("[%s] %s", phase, msg)
}
