package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/drive"
)

// PreclearParams holds the configuration for a single pre-clear job.
type PreclearParams struct {
	FileSystem  string `json:"file_system"`
	ReservedPct int    `json:"reserved_percent"`
	LogDir      string `json:"log_dir"`
}

// PreclearResult holds the final outcome of a pre-clear job.
type PreclearResult struct {
	Passed        bool                    `json:"passed"`
	FailReason    string                  `json:"fail_reason,omitempty"`
	DriveInfo     *drive.DriveInfo        `json:"drive_info"`
	Baseline      *drive.SmartSnapshot    `json:"baseline_snapshot"`
	Partition     *drive.PartitionResult  `json:"partition_result,omitempty"`
	Format        *drive.FormatResult     `json:"format_result,omitempty"`
	Verify        *drive.VerifyResult     `json:"verify_result,omitempty"`
	TotalDuration time.Duration           `json:"total_duration"`
}

// RunPreclear executes the five-phase pre-clear pipeline for a single drive.
// It blocks until completion or context cancellation.
func RunPreclear(ctx context.Context, jobID, devicePath string, params PreclearParams, sink TelemetrySink, logger *slog.Logger) (*PreclearResult, error) {
	result := &PreclearResult{Passed: true}
	emit := newEmitter(sink, jobID, "preclear", logger)

	// ── Phase 1: PRE-FLIGHT ─────────────────────────────────────────────
	emit.phase(PhasePreflight, "resolving device", 0)

	driveInfo, err := drive.ResolveDrive(devicePath)
	if err != nil {
		return nil, failPreclear(emit, result, PhasePreflight, "device resolution failed: %v", err)
	}
	result.DriveInfo = driveInfo

	emit.log(SeverityInfo, "device resolved: %s model=%s serial=%s", driveInfo.Path, driveInfo.Model, driveInfo.Serial)

	// Start periodic alive heartbeat so container logs always show activity.
	emit.startAliveHeartbeat(nil)
	defer emit.stopAliveHeartbeat()

	emit.phase(PhasePreflight, "safety check", 5)

	if err := drive.IsSafeTarget(driveInfo.Path); err != nil {
		return nil, failPreclear(emit, result, PhasePreflight, "safety check failed: %v", err)
	}

	emit.phase(PhasePreflight, "baseline SMART snapshot", 10)

	baseline, err := drive.TakeSnapshot(driveInfo.Path)
	if err != nil {
		return nil, failPreclear(emit, result, PhasePreflight, "baseline snapshot failed: %v", err)
	}
	result.Baseline = baseline

	emit.log(SeverityInfo, "baseline SMART snapshot captured")
	emit.phaseComplete(PhasePreflight)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 2: PARTITION ──────────────────────────────────────────────
	emit.phase(PhasePartition, "wiping partition table", 15)

	fileSystem := params.FileSystem
	if fileSystem == "" {
		fileSystem = "ext4"
	}

	partResult, err := drive.PartitionGPT(ctx, driveInfo.Path, fileSystem, params.ReservedPct)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failPreclear(emit, result, PhasePartition, "partitioning failed: %v", err)
	}
	result.Partition = partResult

	emit.log(SeverityInfo, "GPT partition created: %s", partResult.Partition)
	emit.phaseComplete(PhasePartition)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 3: FORMAT ─────────────────────────────────────────────────
	emit.phase(PhaseFormat, fmt.Sprintf("formatting %s", fileSystem), 30)

	fmtResult, err := drive.FormatPartition(ctx, partResult.Partition, fileSystem, params.ReservedPct, func(progress drive.FormatProgress) {
		// Map format progress (0-100%) into the overall 30-70% band.
		overallPercent := 30.0 + progress.Percent*0.4
		emit.progress(PhaseFormat, progress.Phase, overallPercent, 0, 0, 0, nil)
	}, logger)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failPreclear(emit, result, PhaseFormat, "formatting failed: %v", err)
	}
	result.Format = fmtResult

	emit.log(SeverityInfo, "%s formatted: %s reserved=%d%%", fmtResult.Filesystem, fmtResult.Partition, fmtResult.ReservedPct)
	emit.phaseComplete(PhaseFormat)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 4: VERIFY ─────────────────────────────────────────────────
	emit.phase(PhaseVerify, "verifying filesystem", 75)

	verifyResult, err := drive.VerifyFilesystem(ctx, partResult.Partition, baseline)
	if err != nil {
		return nil, failPreclear(emit, result, PhaseVerify, "verification failed: %v", err)
	}
	result.Verify = verifyResult

	if !verifyResult.MountOK {
		return nil, failPreclear(emit, result, PhaseVerify, "read-only mount verification failed")
	}
	if !verifyResult.MetadataOK {
		return nil, failPreclear(emit, result, PhaseVerify, "filesystem metadata check failed")
	}
	if !verifyResult.SmartOK {
		emit.log(SeverityWarning, "SMART degradation detected after formatting")
		if verifyResult.SmartDelta != nil {
			emit.smartWarning(verifyResult.SmartDelta)
		}
	}

	emit.log(SeverityInfo, "filesystem verification passed")
	emit.phaseComplete(PhaseVerify)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ── Phase 5: COMPLETE ───────────────────────────────────────────────
	result.TotalDuration = time.Since(emit.start)

	// Determine pass/fail.
	if verifyResult.SmartDelta != nil && verifyResult.SmartDelta.Degraded {
		result.Passed = false
		result.FailReason = "SMART attribute degradation after formatting"
	}

	if result.Passed {
		emit.log(SeverityInfo, "pre-clear PASSED for %s (%s) in %s", driveInfo.Path, driveInfo.Serial, result.TotalDuration.Round(time.Second))
		emit.emitComplete("pre-clear passed", 0, nil)
	} else {
		emit.log(SeverityError, "pre-clear FAILED for %s (%s): %s", driveInfo.Path, driveInfo.Serial, result.FailReason)
		var failDeltas json.RawMessage
		if verifyResult.SmartDelta != nil {
			failDeltas = marshalEnrichedDeltas(baseline, nil)
		}
		emit.emitComplete(result.FailReason, 0, failDeltas)
	}

	emit.phaseComplete(PhaseComplete)

	return result, nil
}

// failPreclear marks the result as failed, emits telemetry, and returns an error.
func failPreclear(emit *emitter, result *PreclearResult, phase, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	result.Passed = false
	result.FailReason = msg
	result.TotalDuration = time.Since(emit.start)
	emit.log(SeverityError, "%s", msg)
	emit.emitComplete(msg, 0, nil)
	return fmt.Errorf("[%s] %s", phase, msg)
}
