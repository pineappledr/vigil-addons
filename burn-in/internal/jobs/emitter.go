package jobs

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
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

// aliveInterval controls how often the emitter logs a job-level alive
// heartbeat to stdout / the persistent log file. This makes long-running
// phases (badblocks, SMART extended) clearly visible in container logs.
const aliveInterval = 2 * time.Minute

// emitter wraps TelemetrySink with convenience methods for job orchestrators.
type emitter struct {
	sink    TelemetrySink
	jobID   string
	command string
	start   time.Time
	logger  *slog.Logger
	logFile *JobLogFile // Optional persistent log file for this job.

	// Alive heartbeat state — written by phase(), read by the heartbeat goroutine.
	curPhase  atomic.Value // string
	curTempC  *atomic.Int32
	stopAlive chan struct{}
}

// setLogFile attaches a persistent log file to the emitter. All subsequent
// log() and phase() calls will also write to this file.
func (e *emitter) setLogFile(lf *JobLogFile) {
	e.logFile = lf
}

func newEmitter(sink TelemetrySink, jobID, command string, logger *slog.Logger) *emitter {
	e := &emitter{
		sink:      sink,
		jobID:     jobID,
		command:   command,
		start:     time.Now(),
		logger:    logger,
		stopAlive: make(chan struct{}),
	}
	e.curPhase.Store(PhasePreflight)
	return e
}

// startAliveHeartbeat launches a background goroutine that periodically logs
// a job-level status line. Call stopAliveHeartbeat() to terminate it.
func (e *emitter) startAliveHeartbeat(tempC *atomic.Int32) {
	e.curTempC = tempC
	go func() {
		ticker := time.NewTicker(aliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-e.stopAlive:
				return
			case <-ticker.C:
				phase, _ := e.curPhase.Load().(string)
				elapsed := time.Since(e.start).Round(time.Second)
				temp := int32(0)
				if e.curTempC != nil {
					temp = e.curTempC.Load()
				}
				e.log(SeverityInfo, "ALIVE job=%s phase=%s elapsed=%s temp=%d°C", e.jobID, phase, elapsed, temp)
			}
		}
	}()
}

// stopAliveHeartbeat terminates the background alive heartbeat goroutine.
func (e *emitter) stopAliveHeartbeat() {
	select {
	case <-e.stopAlive:
		// Already closed.
	default:
		close(e.stopAlive)
	}
}

func (e *emitter) elapsed() int64 {
	return int64(time.Since(e.start).Seconds())
}

func (e *emitter) phase(phase, detail string, percent float64) {
	e.curPhase.Store(phase)
	if e.logFile != nil {
		e.logFile.WritePhase(fmt.Sprintf("%s: %s", phase, detail))
	}
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
	// Piggyback a chart frame whenever temperature is available so the
	// drive-temperature line chart updates on every progress tick.
	if tempC > 0 {
		e.chart("drive-temperature", "temp_c", float64(tempC))
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
	if e.logFile != nil {
		e.logFile.WriteLog(severity, "%s", msg)
	}
	if e.sink == nil {
		return
	}
	if err := e.sink.SendLog(e.jobID, severity, msg); err != nil {
		e.logger.Warn("failed to transmit log", "error", err)
	}
}

func (e *emitter) phaseComplete(phase string) {
	if e.logFile != nil {
		e.logFile.WritePhase(fmt.Sprintf("Finished %s", phase))
	}
	e.log(SeverityInfo, "phase %s complete", phase)
}

func (e *emitter) smartWarning(delta *drive.SmartDelta) {
	data, err := json.Marshal(delta.Deltas)
	if err != nil {
		return
	}
	e.progress("SMART_WARNING", "attribute degradation detected", 0, 0, 0, 0, data)
}

// marshalEnrichedDeltas converts baseline and current snapshots into the
// enriched JSON format expected by the Dashboard SMART Attribute Deltas table.
func marshalEnrichedDeltas(baseline, current *drive.SmartSnapshot) json.RawMessage {
	enriched := drive.EnrichedDeltas(baseline, current)
	data, err := json.Marshal(enriched)
	if err != nil {
		return nil
	}
	return data
}

func (e *emitter) emitComplete(detail string, badblockErrs int, enrichedDeltas json.RawMessage) {
	e.progress(PhaseComplete, detail, 100, 0, 0, badblockErrs, enrichedDeltas)
}
