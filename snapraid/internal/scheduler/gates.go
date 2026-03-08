package scheduler

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
	"github.com/pineappledr/vigil-addons/snapraid/internal/engine"
)

// GateResult holds the outcome of a pre-flight safety check.
type GateResult struct {
	Passed  bool
	Gate    string
	Reason  string
}

// CheckConfigFiles validates that all content and parity files referenced
// in the snapraid configuration exist and are non-empty (Gate 0).
func CheckConfigFiles(eng *engine.Engine) GateResult {
	if err := eng.ValidateConfigFiles(); err != nil {
		return GateResult{
			Gate:   "config_files",
			Reason: err.Error(),
		}
	}
	return GateResult{Passed: true, Gate: "config_files"}
}

// CheckSMART evaluates the SmartReport against configured thresholds (Gate 1).
// Returns a failed GateResult if any disk reports FAIL/PREFAIL or exceeds the
// configured failure probability threshold.
func CheckSMART(report *engine.SmartReport, thresholds config.Thresholds) GateResult {
	for _, d := range report.Disks {
		status := strings.ToUpper(d.Status)
		if status == "FAIL" || status == "PREFAIL" {
			return GateResult{
				Gate:   "smart",
				Reason: fmt.Sprintf("disk %s (%s) reports %s", d.DiskName, d.Device, d.Status),
			}
		}
		if thresholds.SmartFailProbability > 0 && d.FailureProbability >= float64(thresholds.SmartFailProbability) {
			return GateResult{
				Gate:   "smart",
				Reason: fmt.Sprintf("disk %s (%s) failure probability %.0f%% exceeds threshold %d%%", d.DiskName, d.Device, d.FailureProbability, thresholds.SmartFailProbability),
			}
		}
	}
	return GateResult{Passed: true, Gate: "smart"}
}

// CheckDiffThresholds evaluates the DiffReport against configured thresholds (Gate 2).
// Returns a failed GateResult if deletions or updates exceed their limits.
// The add_del_ratio can override a deletion breach if the ratio of added-to-deleted is high enough.
func CheckDiffThresholds(report *engine.DiffReport, thresholds config.Thresholds) GateResult {
	// Check deleted threshold
	if thresholds.MaxDeleted >= 0 && report.Removed > thresholds.MaxDeleted {
		// Check if add/del ratio override applies
		if thresholds.AddDelRatio > 0 && report.Removed > 0 {
			ratio := float64(report.Added) / float64(report.Removed)
			if ratio >= thresholds.AddDelRatio {
				// Ratio override: allow despite deletion breach
				return GateResult{Passed: true, Gate: "diff_thresholds"}
			}
		}
		return GateResult{
			Gate:   "diff_thresholds",
			Reason: fmt.Sprintf("%d files deleted exceeds threshold of %d; manual sync required", report.Removed, thresholds.MaxDeleted),
		}
	}

	// Check updated threshold (disabled when -1)
	if thresholds.MaxUpdated >= 0 && report.Updated > thresholds.MaxUpdated {
		return GateResult{
			Gate:   "diff_thresholds",
			Reason: fmt.Sprintf("%d files updated exceeds threshold of %d", report.Updated, thresholds.MaxUpdated),
		}
	}

	return GateResult{Passed: true, Gate: "diff_thresholds"}
}

// ErrEngineBusy is returned when Gate 3 (concurrency lock) detects an active operation.
var ErrEngineBusy = errors.New("another operation is in progress")

// CheckLock verifies that the engine mutex is available (Gate 3).
// This is checked implicitly when the engine's TryLock fails, but this gate
// provides a consistent GateResult interface for pipeline reporting.
func CheckLock(err error) GateResult {
	if errors.Is(err, engine.ErrEngineLocked) {
		return GateResult{
			Gate:   "concurrency_lock",
			Reason: "another snapraid operation is currently running",
		}
	}
	return GateResult{Passed: true, Gate: "concurrency_lock"}
}
