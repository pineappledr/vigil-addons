package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/drive"
)

// BurninParams holds the configuration for a single burn-in job.
type BurninParams struct {
	BlockSize        int    `json:"block_size"`
	ConcurrentBlocks int    `json:"concurrent_blocks"`
	AbortOnError     bool   `json:"abort_on_error"`
	LogDir           string `json:"log_dir"`
}

// BurninResult holds the final outcome of a burn-in job.
type BurninResult struct {
	Passed        bool                 `json:"passed"`
	FailReason    string               `json:"fail_reason,omitempty"`
	DriveInfo     *drive.DriveInfo     `json:"drive_info"`
	Baseline      *drive.SmartSnapshot `json:"baseline_snapshot"`
	FinalSnapshot *drive.SmartSnapshot `json:"final_snapshot"`
	FinalDelta    *drive.SmartDelta    `json:"final_delta"`
	BadblockErrs  int                  `json:"badblock_errors"`
	ShortTest     *drive.TestResult    `json:"short_test"`
	LongTest      *drive.TestResult    `json:"long_test"`
	TotalDuration time.Duration        `json:"total_duration"`
}

// burninJob holds shared state for a burn-in pipeline execution.
type burninJob struct {
	ctx          context.Context
	params       BurninParams
	emit         *emitter
	logFile      *JobLogFile
	result       *BurninResult
	baseline     *drive.SmartSnapshot
	latestDeltas json.RawMessage
	currentTempC atomic.Int32
	tempCancel   context.CancelFunc
}

// RunBurnin executes the full five-phase burn-in pipeline for a single drive.
// It blocks until completion or context cancellation.
func RunBurnin(ctx context.Context, jobID, devicePath string, params BurninParams, sink TelemetrySink, logger *slog.Logger) (*BurninResult, error) {
	j := &burninJob{
		ctx:    ctx,
		params: params,
		emit:   newEmitter(sink, jobID, "burnin", logger),
		result: &BurninResult{Passed: true},
	}

	if err := j.preflight(devicePath, logger); err != nil {
		return j.result, err
	}
	defer j.logFile.Close()
	defer j.tempCancel()
	defer j.emit.stopAliveHeartbeat()

	if err := j.smartShort(); err != nil {
		return j.result, err
	}

	if err := j.badblocks(); err != nil {
		return j.result, err
	}

	if err := j.smartExtended(); err != nil {
		return j.result, err
	}

	j.complete()

	if j.logFile != nil {
		j.logFile.WriteResult(j.result.Passed, j.result.FailReason, j.result.BadblockErrs, j.result.TotalDuration)
	}

	return j.result, nil
}

// preflight resolves the device, captures a baseline SMART snapshot,
// and starts background temperature polling and heartbeat.
func (j *burninJob) preflight(devicePath string, logger *slog.Logger) error {
	j.emit.phase(PhasePreflight, "resolving device", 0)

	driveInfo, err := drive.ResolveDrive(devicePath)
	if err != nil {
		return failJob(j.emit, j.result, PhasePreflight, nil, "device resolution failed: %v", err)
	}
	j.result.DriveInfo = driveInfo

	logFile, err := NewJobLogFile(j.params.LogDir, j.emit.jobID, driveInfo)
	if err != nil {
		logger.Warn("failed to create job log file, continuing without file logging", "error", err)
	}
	j.logFile = logFile

	if logFile != nil {
		logFile.WriteHeader(j.emit.jobID, driveInfo)
		j.emit.setLogFile(logFile)
	}

	j.emit.log(SeverityInfo, "device resolved: %s model=%s serial=%s rotation=%s", driveInfo.Path, driveInfo.Model, driveInfo.Serial, driveInfo.Rotation)

	// Background temperature poller — polls smartctl -A every 60s.
	var tempCtx context.Context
	tempCtx, j.tempCancel = context.WithCancel(j.ctx)
	go j.pollTemperature(tempCtx, driveInfo.Path)

	j.emit.startAliveHeartbeat(&j.currentTempC)

	j.emit.phase(PhasePreflight, "safety check", 5)
	if err := drive.IsSafeTarget(driveInfo.Path); err != nil {
		return failJob(j.emit, j.result, PhasePreflight, nil, "safety check failed: %v", err)
	}

	j.emit.phase(PhasePreflight, "baseline SMART snapshot", 10)
	baseline, err := drive.TakeSnapshot(driveInfo.Path)
	if err != nil {
		return failJob(j.emit, j.result, PhasePreflight, nil, "baseline snapshot failed: %v", err)
	}
	j.baseline = baseline
	j.result.Baseline = baseline
	j.latestDeltas = marshalEnrichedDeltas(baseline, baseline)

	j.emit.log(SeverityInfo, "baseline SMART snapshot captured")
	j.emit.progress(PhasePreflight, "baseline captured", 12, 0, int(j.currentTempC.Load()), 0, j.latestDeltas)
	j.emit.phaseComplete(PhasePreflight)

	return j.ctx.Err()
}

// pollTemperature reads the drive temperature every 60s until the context is cancelled.
func (j *burninJob) pollTemperature(ctx context.Context, devicePath string) {
	if t, err := drive.ReadTemperature(devicePath); err == nil && t > 0 {
		j.currentTempC.Store(int32(t))
		j.emit.chart("drive-temperature", "temp_c", float64(t))
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if t, err := drive.ReadTemperature(devicePath); err == nil && t > 0 {
				j.currentTempC.Store(int32(t))
				j.emit.chart("drive-temperature", "temp_c", float64(t))
			}
		}
	}
}

// smartShort runs the SMART short self-test and captures a post-test snapshot.
func (j *burninJob) smartShort() error {
	j.emit.phase(PhaseSmartShort, "starting short test", 15)
	j.emit.log(SeverityInfo, "starting SMART short test on %s (typically takes 1-2 minutes)", j.result.DriveInfo.Path)

	shortResult, err := drive.RunShortTest(j.ctx, j.result.DriveInfo.Path, func(pct float64, elapsed time.Duration, msg string) {
		overallPct := 15.0 + pct*0.10
		tempC := int(j.currentTempC.Load())
		j.emit.progress(PhaseSmartShort, msg, overallPct, 0, tempC, 0, j.latestDeltas)
		j.emit.log(SeverityInfo, "SMART_SHORT heartbeat: %.1f%% elapsed=%s temp=%d°C", pct, elapsed.Round(time.Second), tempC)
	})
	if err != nil {
		return failJob(j.emit, j.result, PhaseSmartShort, j.latestDeltas, "short test failed: %v", err)
	}
	j.result.ShortTest = shortResult

	if !shortResult.Passed {
		j.emit.log(SeverityWarning, "SMART short test reported failure: %s", shortResult.Status)
	}

	j.emit.phase(PhaseSmartShort, "post-short snapshot", 20)

	postShort, err := drive.TakeSnapshot(j.result.DriveInfo.Path)
	if err != nil {
		j.emit.log(SeverityWarning, "post-short snapshot failed: %v", err)
	} else {
		j.latestDeltas = marshalEnrichedDeltas(j.baseline, postShort)
		shortDelta := drive.ComputeDelta(j.baseline, postShort)
		if shortDelta.Degraded {
			j.emit.log(SeverityWarning, "SMART degradation detected after short test")
			j.emit.smartWarning(shortDelta)
		}
	}

	j.emit.progress(PhaseSmartShort, "post-short snapshot", 24, 0, int(j.currentTempC.Load()), 0, j.latestDeltas)
	j.emit.phaseComplete(PhaseSmartShort)

	return j.ctx.Err()
}

// badblocks runs the destructive badblocks test (skipped for SSDs).
func (j *burninJob) badblocks() error {
	j.emit.phase(PhaseBadblocks, "preparing", 25)
	j.emit.log(SeverityInfo, "starting BADBLOCKS phase on %s (duration depends on drive size, may take many hours for large drives)", j.result.DriveInfo.Path)

	if j.result.DriveInfo.IsSSD() {
		j.emit.log(SeverityInfo, "SKIPPED: badblocks for %s device (SSD detected)", j.result.DriveInfo.Rotation)
		j.emit.phaseComplete(PhaseBadblocks)
		return j.ctx.Err()
	}

	if j.params.LogDir != "" {
		if err := drive.EnsureLogDir(j.params.LogDir); err != nil {
			return failJob(j.emit, j.result, PhaseBadblocks, j.latestDeltas, "cannot create log dir: %v", err)
		}
	}

	var lastBBLogTime time.Time
	var lastBBLogPct float64
	var lastDeltaTime time.Time
	liveDeltaJSON := j.latestDeltas
	bbResult, err := drive.RunBadblocks(j.ctx, j.result.DriveInfo.Path, j.params.BlockSize, j.params.ConcurrentBlocks, j.params.AbortOnError, j.params.LogDir, func(progress drive.BadblocksProgress) {
		now := time.Now()
		tempC := int(j.currentTempC.Load())

		// Refresh SMART deltas every 5 minutes during badblocks.
		if now.Sub(lastDeltaTime) >= 5*time.Minute {
			if snap, err := drive.TakeSnapshot(j.result.DriveInfo.Path); err == nil {
				delta := drive.ComputeDelta(j.baseline, snap)
				liveDeltaJSON = marshalEnrichedDeltas(j.baseline, snap)
				if delta.Degraded {
					j.emit.log(SeverityWarning, "SMART degradation detected during badblocks")
				}
			}
			lastDeltaTime = now
		}

		overallPercent := 25.0 + progress.Percent*0.5
		j.emit.progress(PhaseBadblocks, progress.Phase, overallPercent, 0, tempC, progress.Errors, liveDeltaJSON)

		// Throttled local log heartbeat: every 30s or every 1% progress.
		if now.Sub(lastBBLogTime) >= 30*time.Second || progress.Percent-lastBBLogPct >= 1.0 {
			eta := ""
			if progress.Percent > 0 && progress.Percent < 100 {
				phaseElapsed := time.Since(j.emit.start)
				estTotal := time.Duration(float64(phaseElapsed) / (progress.Percent / 100.0))
				remaining := estTotal - phaseElapsed
				if remaining > 0 {
					eta = fmt.Sprintf(" ETA=%s", remaining.Round(time.Second))
				}
			}
			j.emit.log(SeverityInfo, "BADBLOCKS heartbeat: %.1f%% (%s) errors=%d temp=%d°C elapsed=%ds%s",
				progress.Percent, progress.Phase, progress.Errors, tempC, j.emit.elapsed(), eta)
			lastBBLogTime = now
			lastBBLogPct = progress.Percent
		}
	})
	if err != nil {
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		return failJob(j.emit, j.result, PhaseBadblocks, j.latestDeltas, "badblocks failed: %v", err)
	}

	j.result.BadblockErrs = bbResult.Errors
	j.latestDeltas = liveDeltaJSON

	if bbResult.Errors > 0 {
		j.emit.log(SeverityError, "badblocks found %d error(s)", bbResult.Errors)
		if j.params.AbortOnError {
			j.result.Passed = false
			j.result.FailReason = fmt.Sprintf("badblocks found %d error(s)", bbResult.Errors)
			j.emit.phaseComplete(PhaseBadblocks)
			return failJob(j.emit, j.result, PhaseBadblocks, j.latestDeltas, "%s", j.result.FailReason)
		}
	} else {
		j.emit.log(SeverityInfo, "badblocks completed with zero errors")
	}

	j.emit.phaseComplete(PhaseBadblocks)
	return j.ctx.Err()
}

// smartExtended runs the SMART extended self-test and captures the final snapshot.
func (j *burninJob) smartExtended() error {
	j.emit.phase(PhaseSmartExtended, "starting extended test", 75)
	j.emit.log(SeverityInfo, "starting SMART extended test on %s (typically takes 1-8 hours depending on drive capacity)", j.result.DriveInfo.Path)

	longResult, err := drive.RunLongTest(j.ctx, j.result.DriveInfo.Path, func(pct float64, elapsed time.Duration, msg string) {
		overallPct := 75.0 + pct*0.20
		tempC := int(j.currentTempC.Load())
		eta := ""
		if pct > 0 && pct < 100 {
			estTotal := time.Duration(float64(elapsed) / (pct / 100.0))
			remaining := estTotal - elapsed
			if remaining > 0 {
				eta = fmt.Sprintf(" ETA=%s", remaining.Round(time.Second))
			}
		}
		j.emit.progress(PhaseSmartExtended, msg, overallPct, 0, tempC, 0, j.latestDeltas)
		j.emit.log(SeverityInfo, "SMART_EXTENDED heartbeat: %.1f%% elapsed=%s temp=%d°C%s", pct, elapsed.Round(time.Second), tempC, eta)
	})
	if err != nil {
		return failJob(j.emit, j.result, PhaseSmartExtended, j.latestDeltas, "extended test failed: %v", err)
	}
	j.result.LongTest = longResult

	if !longResult.Passed {
		j.emit.log(SeverityWarning, "SMART extended test reported failure: %s", longResult.Status)
	}

	j.emit.phase(PhaseSmartExtended, "final snapshot", 90)

	finalSnap, err := drive.TakeSnapshot(j.result.DriveInfo.Path)
	if err != nil {
		j.emit.log(SeverityWarning, "final SMART snapshot failed: %v", err)
	} else {
		j.result.FinalSnapshot = finalSnap
		j.result.FinalDelta = drive.ComputeDelta(j.baseline, finalSnap)
		j.latestDeltas = marshalEnrichedDeltas(j.baseline, finalSnap)

		if j.result.FinalDelta.Degraded {
			j.emit.log(SeverityWarning, "SMART degradation detected in final snapshot")
			j.emit.smartWarning(j.result.FinalDelta)
		}
	}

	j.emit.progress(PhaseSmartExtended, "final snapshot", 94, 0, int(j.currentTempC.Load()), 0, j.latestDeltas)
	j.emit.phaseComplete(PhaseSmartExtended)

	return j.ctx.Err()
}

// complete aggregates pass/fail results from all phases and emits the final status.
func (j *burninJob) complete() {
	j.result.TotalDuration = time.Since(j.emit.start)

	if !j.result.ShortTest.Passed || (j.result.LongTest != nil && !j.result.LongTest.Passed) {
		j.result.Passed = false
		j.result.FailReason = "SMART self-test failure"
	}
	if j.result.FinalDelta != nil && j.result.FinalDelta.Degraded {
		j.result.Passed = false
		if j.result.FailReason != "" {
			j.result.FailReason += "; "
		}
		j.result.FailReason += "SMART attribute degradation"
	}
	if j.result.BadblockErrs > 0 {
		j.result.Passed = false
		if j.result.FailReason != "" {
			j.result.FailReason += "; "
		}
		j.result.FailReason += fmt.Sprintf("%d bad block(s)", j.result.BadblockErrs)
	}

	if j.result.Passed {
		j.emit.log(SeverityInfo, "burn-in PASSED for %s (%s) in %s", j.result.DriveInfo.Path, j.result.DriveInfo.Serial, j.result.TotalDuration.Round(time.Second))
		j.emit.emitComplete("burn-in passed", j.result.BadblockErrs, j.latestDeltas)
	} else {
		j.emit.log(SeverityError, "burn-in FAILED for %s (%s): %s", j.result.DriveInfo.Path, j.result.DriveInfo.Serial, j.result.FailReason)
		j.emit.emitComplete(j.result.FailReason, j.result.BadblockErrs, j.latestDeltas)
	}

	j.emit.phaseComplete(PhaseComplete)
}

// failJob marks the result as failed, emits telemetry, and returns an error.
func failJob(emit *emitter, result *BurninResult, phase string, enrichedDeltas json.RawMessage, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	result.Passed = false
	result.FailReason = msg
	result.TotalDuration = time.Since(emit.start)
	emit.log(SeverityError, "%s", msg)
	emit.emitComplete(msg, result.BadblockErrs, enrichedDeltas)
	return fmt.Errorf("[%s] %s", phase, msg)
}
