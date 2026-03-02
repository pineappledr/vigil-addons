package jobs

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/drive"
)

// Phase constants shared across job orchestrators.
const (
	PhasePreflight     = "PRE-FLIGHT"
	PhaseSmartShort    = "SMART_SHORT"
	PhaseBadblocks     = "BADBLOCKS"
	PhaseSmartExtended = "SMART_EXTENDED"
	PhasePartition     = "PARTITION"
	PhaseFormat        = "FORMAT"
	PhaseVerify        = "VERIFY"
	PhaseComplete      = "COMPLETE"
)

// Severity levels for telemetry log frames.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// TelemetrySink is the interface for emitting telemetry frames during a job.
// This decouples the orchestrator from the concrete HubTelemetry client.
type TelemetrySink interface {
	SendProgress(jobID, command, phase, phaseDetail string, percent, speedMbps float64, tempC int, elapsedSec, etaSec int64, badblockErrs int, smartDeltas json.RawMessage) error
	SendLog(jobID, severity, message string) error
	SendMetric(key string, value float64) error
	SendChart(componentID, key string, value float64) error
}

// emitter wraps TelemetrySink with convenience methods for job orchestrators.
type emitter struct {
	sink    TelemetrySink
	jobID   string
	command string
	start   time.Time
	logger  *slog.Logger
}

func newEmitter(sink TelemetrySink, jobID, command string, logger *slog.Logger) *emitter {
	return &emitter{
		sink:    sink,
		jobID:   jobID,
		command: command,
		start:   time.Now(),
		logger:  logger,
	}
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
	etaSec := e.estimateETA(percent)
	if err := e.sink.SendProgress(e.jobID, e.command, phase, detail, percent, speedMbps, tempC, e.elapsed(), etaSec, badblockErrs, smartDeltas); err != nil {
		e.logger.Warn("failed to transmit progress", "error", err, "phase", phase)
	}
}

// estimateETA calculates the remaining seconds based on elapsed time and
// overall completion percentage. Returns 0 if estimation is not meaningful.
func (e *emitter) estimateETA(percent float64) int64 {
	if percent <= 0 || percent >= 100 {
		return 0
	}
	elapsedSec := float64(time.Since(e.start).Seconds())
	if elapsedSec < 5 {
		return 0 // Too early for a meaningful estimate.
	}
	totalEstimate := elapsedSec / (percent / 100.0)
	remaining := totalEstimate - elapsedSec
	if remaining < 0 {
		return 0
	}
	return int64(remaining)
}

func (e *emitter) metric(key string, value float64) {
	if e.sink == nil {
		return
	}
	if err := e.sink.SendMetric(key, value); err != nil {
		e.logger.Warn("failed to transmit metric", "error", err, "key", key)
	}
}

func (e *emitter) chart(componentID, key string, value float64) {
	if e.sink == nil {
		return
	}
	if err := e.sink.SendChart(componentID, key, value); err != nil {
		e.logger.Warn("failed to transmit chart point", "error", err, "component_id", componentID, "key", key)
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

func (e *emitter) emitComplete(detail string, badblockErrs int, finalDelta *drive.SmartDelta) {
	var deltaJSON json.RawMessage
	if finalDelta != nil {
		deltaJSON, _ = json.Marshal(finalDelta.Deltas)
	}
	e.progress(PhaseComplete, detail, 100, 0, 0, badblockErrs, deltaJSON)
}
