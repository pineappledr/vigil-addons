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

// RunBurnin executes the full five-phase burn-in pipeline for a single drive.
// It blocks until completion or context cancellation.
func RunBurnin(ctx context.Context, jobID, devicePath string, params BurninParams, sink TelemetrySink, logger *slog.Logger) (*BurninResult, error) {
	result := &BurninResult{Passed: true}
	emit := newEmitter(sink, jobID, "burnin", logger)

	// ── Phase 1: PRE-FLIGHT ─────────────────────────────────────────────
	emit.phase(PhasePreflight, "resolving device", 0)

	driveInfo, err := drive.ResolveDrive(devicePath)
	if err != nil {
		return nil, failJob(emit, result, PhasePreflight, nil, "device resolution failed: %v", err)
	}
	result.DriveInfo = driveInfo

	// Open persistent log file for this job (Spearfoot-style per-drive log).
	logFile, err := NewJobLogFile(params.LogDir, jobID, driveInfo)
	if err != nil {
		logger.Warn("failed to create job log file, continuing without file logging", "error", err)
	}
	defer logFile.Close()

	if logFile != nil {
		logFile.WriteHeader(jobID, driveInfo)
		emit.setLogFile(logFile)
	}

	emit.log(SeverityInfo, "device resolved: %s model=%s serial=%s rotation=%s", driveInfo.Path, driveInfo.Model, driveInfo.Serial, driveInfo.Rotation)

	// ── Background temperature poller ──────────────────────────────────
	// Polls smartctl -A every 60s to retrieve the current drive temperature.
	// The value is stored atomically and injected into progress frames.
	var currentTempC atomic.Int32
	tempCtx, tempCancel := context.WithCancel(ctx)
	defer tempCancel()
	go func() {
		// Retrieve initial temperature immediately.
		if t, err := drive.ReadTemperature(driveInfo.Path); err == nil && t > 0 {
			currentTempC.Store(int32(t))
			emit.chart("drive-temperature", "temp_c", float64(t))
		}
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-tempCtx.Done():
				return
			case <-ticker.C:
				if t, err := drive.ReadTemperature(driveInfo.Path); err == nil && t > 0 {
					currentTempC.Store(int32(t))
					emit.chart("drive-temperature", "temp_c", float64(t))
				}
			}
		}
	}()

	// Start periodic alive heartbeat so container logs always show activity.
	emit.startAliveHeartbeat(&currentTempC)
	defer emit.stopAliveHeartbeat()

	emit.phase(PhasePreflight, "safety check", 5)

	if err := drive.IsSafeTarget(driveInfo.Path); err != nil {
		return nil, failJob(emit, result, PhasePreflight, nil, "safety check failed: %v", err)
	}

	emit.phase(PhasePreflight, "baseline SMART snapshot", 10)

	baseline, err := drive.TakeSnapshot(driveInfo.Path)
	if err != nil {
		return nil, failJob(emit, result, PhasePreflight, nil, "baseline snapshot failed: %v", err)
	}
	result.Baseline = baseline

	emit.log(SeverityInfo, "baseline SMART snapshot captured")

	// Send initial baseline deltas so the Dashboard table shows current values
	// even before any test phase completes.
	baselineDeltas := marshalEnrichedDeltas(baseline, baseline)
	emit.progress(PhasePreflight, "baseline captured", 12, 0, int(currentTempC.Load()), 0, baselineDeltas)

	emit.phaseComplete(PhasePreflight)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 2: SMART SHORT ────────────────────────────────────────────
	emit.phase(PhaseSmartShort, "starting short test", 15)
	emit.log(SeverityInfo, "starting SMART short test on %s (typically takes 1-2 minutes)", driveInfo.Path)

	shortResult, err := drive.RunShortTest(ctx, driveInfo.Path, func(pct float64, elapsed time.Duration, msg string) {
		// Map short test progress (0-100%) into the overall 15-25% band.
		overallPct := 15.0 + pct*0.10
		tempC := int(currentTempC.Load())
		emit.progress(PhaseSmartShort, msg, overallPct, 0, tempC, 0, baselineDeltas)
		emit.log(SeverityInfo, "SMART_SHORT heartbeat: %.1f%% elapsed=%s temp=%d°C", pct, elapsed.Round(time.Second), tempC)
	})
	if err != nil {
		return nil, failJob(emit, result, PhaseSmartShort, baselineDeltas, "short test failed: %v", err)
	}
	result.ShortTest = shortResult

	if !shortResult.Passed {
		emit.log(SeverityWarning, "SMART short test reported failure: %s", shortResult.Status)
	}

	emit.phase(PhaseSmartShort, "post-short snapshot", 20)

	postShort, err := drive.TakeSnapshot(driveInfo.Path)
	var latestDeltas json.RawMessage = baselineDeltas
	if err != nil {
		emit.log(SeverityWarning, "post-short snapshot failed: %v", err)
	} else {
		latestDeltas = marshalEnrichedDeltas(baseline, postShort)
		shortDelta := drive.ComputeDelta(baseline, postShort)
		if shortDelta.Degraded {
			emit.log(SeverityWarning, "SMART degradation detected after short test")
			emit.smartWarning(shortDelta)
		}
	}

	// Send post-short enriched deltas to update the Dashboard table.
	emit.progress(PhaseSmartShort, "post-short snapshot", 24, 0, int(currentTempC.Load()), 0, latestDeltas)
	emit.phaseComplete(PhaseSmartShort)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 3: BADBLOCKS ──────────────────────────────────────────────
	emit.phase(PhaseBadblocks, "preparing", 25)
	emit.log(SeverityInfo, "starting BADBLOCKS phase on %s (duration depends on drive size, may take many hours for large drives)", driveInfo.Path)

	// Skip badblocks for SSDs — destructive write patterns waste flash write
	// endurance without providing meaningful testing due to wear leveling.
	// This matches the behaviour of the Spearfoot disk-burnin.sh script.
	if driveInfo.IsSSD() {
		emit.log(SeverityInfo, "SKIPPED: badblocks for %s device (SSD detected)", driveInfo.Rotation)
		emit.phaseComplete(PhaseBadblocks)
	} else {
		if params.LogDir != "" {
			if err := drive.EnsureLogDir(params.LogDir); err != nil {
				return nil, failJob(emit, result, PhaseBadblocks, latestDeltas, "cannot create log dir: %v", err)
			}
		}

		var lastBBLogTime time.Time
		var lastBBLogPct float64
		var lastDeltaTime time.Time
		liveDeltaJSON := latestDeltas
		bbResult, err := drive.RunBadblocks(ctx, driveInfo.Path, params.BlockSize, params.ConcurrentBlocks, params.AbortOnError, params.LogDir, func(progress drive.BadblocksProgress) {
			now := time.Now()
			tempC := int(currentTempC.Load())

			// Refresh SMART deltas every 5 minutes during badblocks.
			if now.Sub(lastDeltaTime) >= 5*time.Minute {
				if snap, err := drive.TakeSnapshot(driveInfo.Path); err == nil {
					delta := drive.ComputeDelta(baseline, snap)
					liveDeltaJSON = marshalEnrichedDeltas(baseline, snap)
					if delta.Degraded {
						emit.log(SeverityWarning, "SMART degradation detected during badblocks")
					}
				}
				lastDeltaTime = now
			}

			// Map badblocks progress (0-100%) into the overall 25-75% band.
			overallPercent := 25.0 + progress.Percent*0.5
			emit.progress(PhaseBadblocks, progress.Phase, overallPercent, 0, tempC, progress.Errors, liveDeltaJSON)

			// Throttled local log heartbeat: every 30s or every 1% progress.
			if now.Sub(lastBBLogTime) >= 30*time.Second || progress.Percent-lastBBLogPct >= 1.0 {
				eta := ""
				if progress.Percent > 0 && progress.Percent < 100 {
					phaseElapsed := time.Since(emit.start)
					estTotal := time.Duration(float64(phaseElapsed) / (progress.Percent / 100.0))
					remaining := estTotal - phaseElapsed
					if remaining > 0 {
						eta = fmt.Sprintf(" ETA=%s", remaining.Round(time.Second))
					}
				}
				emit.log(SeverityInfo, "BADBLOCKS heartbeat: %.1f%% (%s) errors=%d temp=%d°C elapsed=%ds%s",
					progress.Percent, progress.Phase, progress.Errors, tempC, emit.elapsed(), eta)
				lastBBLogTime = now
				lastBBLogPct = progress.Percent
			}
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, failJob(emit, result, PhaseBadblocks, latestDeltas, "badblocks failed: %v", err)
		}

		result.BadblockErrs = bbResult.Errors

		if bbResult.Errors > 0 {
			emit.log(SeverityError, "badblocks found %d error(s)", bbResult.Errors)
			if params.AbortOnError {
				result.Passed = false
				result.FailReason = fmt.Sprintf("badblocks found %d error(s)", bbResult.Errors)
				emit.phaseComplete(PhaseBadblocks)
				return result, failJob(emit, result, PhaseBadblocks, latestDeltas, "%s", result.FailReason)
			}
		} else {
			emit.log(SeverityInfo, "badblocks completed with zero errors")
		}

		emit.phaseComplete(PhaseBadblocks)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 4: SMART EXTENDED ─────────────────────────────────────────
	emit.phase(PhaseSmartExtended, "starting extended test", 75)
	emit.log(SeverityInfo, "starting SMART extended test on %s (typically takes 1-8 hours depending on drive capacity)", driveInfo.Path)

	longResult, err := drive.RunLongTest(ctx, driveInfo.Path, func(pct float64, elapsed time.Duration, msg string) {
		// Map extended test progress (0-100%) into the overall 75-95% band.
		overallPct := 75.0 + pct*0.20
		tempC := int(currentTempC.Load())
		eta := ""
		if pct > 0 && pct < 100 {
			estTotal := time.Duration(float64(elapsed) / (pct / 100.0))
			remaining := estTotal - elapsed
			if remaining > 0 {
				eta = fmt.Sprintf(" ETA=%s", remaining.Round(time.Second))
			}
		}
		emit.progress(PhaseSmartExtended, msg, overallPct, 0, tempC, 0, latestDeltas)
		emit.log(SeverityInfo, "SMART_EXTENDED heartbeat: %.1f%% elapsed=%s temp=%d°C%s", pct, elapsed.Round(time.Second), tempC, eta)
	})
	if err != nil {
		return nil, failJob(emit, result, PhaseSmartExtended, latestDeltas, "extended test failed: %v", err)
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
		latestDeltas = marshalEnrichedDeltas(baseline, finalSnap)

		if result.FinalDelta.Degraded {
			emit.log(SeverityWarning, "SMART degradation detected in final snapshot")
			emit.smartWarning(result.FinalDelta)
		}
	}

	// Send final enriched deltas to update the Dashboard table.
	emit.progress(PhaseSmartExtended, "final snapshot", 94, 0, int(currentTempC.Load()), 0, latestDeltas)
	emit.phaseComplete(PhaseSmartExtended)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 5: COMPLETE ───────────────────────────────────────────────
	result.TotalDuration = time.Since(emit.start)

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
	if result.BadblockErrs > 0 {
		result.Passed = false
		if result.FailReason != "" {
			result.FailReason += "; "
		}
		result.FailReason += fmt.Sprintf("%d bad block(s)", result.BadblockErrs)
	}

	if result.Passed {
		emit.log(SeverityInfo, "burn-in PASSED for %s (%s) in %s", driveInfo.Path, driveInfo.Serial, result.TotalDuration.Round(time.Second))
		emit.emitComplete("burn-in passed", result.BadblockErrs, latestDeltas)
	} else {
		emit.log(SeverityError, "burn-in FAILED for %s (%s): %s", driveInfo.Path, driveInfo.Serial, result.FailReason)
		emit.emitComplete(result.FailReason, result.BadblockErrs, latestDeltas)
	}

	// Write final result summary to the persistent log file.
	if logFile != nil {
		logFile.WriteResult(result.Passed, result.FailReason, result.BadblockErrs, result.TotalDuration)
	}

	emit.phaseComplete(PhaseComplete)

	return result, nil
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
