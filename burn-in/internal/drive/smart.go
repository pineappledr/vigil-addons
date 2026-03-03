package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Critical SMART attribute IDs for degradation detection.
const (
	AttrReallocatedSectors   = 5
	AttrReallocatedEvents    = 196
	AttrCurrentPendingSector = 197
	AttrOfflineUncorrectable = 198
)

// criticalAttrs is the ordered set of SMART attribute IDs tracked for deltas.
var criticalAttrs = []int{
	AttrReallocatedSectors,
	AttrReallocatedEvents,
	AttrCurrentPendingSector,
	AttrOfflineUncorrectable,
}

// SmartSnapshot holds raw values for critical SMART attributes at a point in time.
type SmartSnapshot struct {
	Timestamp  time.Time      `json:"timestamp"`
	DevicePath string         `json:"device_path"`
	Attrs      map[int]int64  `json:"attrs"` // Attribute ID → raw value.
}

// SmartDelta holds the change in critical SMART attributes between two snapshots.
type SmartDelta struct {
	DevicePath string         `json:"device_path"`
	Deltas     map[int]int64  `json:"deltas"`     // Attribute ID → change in raw value.
	Degraded   bool           `json:"degraded"`   // True if any critical attribute increased.
}

// attrNames maps critical SMART attribute IDs to human-readable names.
var attrNames = map[int]string{
	AttrReallocatedSectors:   "Reallocated_Sector_Ct",
	AttrReallocatedEvents:    "Reallocated_Event_Count",
	AttrCurrentPendingSector: "Current_Pending_Sector",
	AttrOfflineUncorrectable: "Offline_Uncorrectable",
}

// EnrichedAttr is a single SMART attribute with baseline, current value,
// and human-readable name — formatted for the Dashboard UI table.
type EnrichedAttr struct {
	Name     string `json:"name"`
	Baseline int64  `json:"baseline"`
	Current  int64  `json:"current"`
}

// EnrichedDeltas converts a SmartDelta into the map format expected by the
// Vigil Dashboard UI: { "5": { name, baseline, current }, ... }.
func EnrichedDeltas(baseline, current *SmartSnapshot) map[string]EnrichedAttr {
	result := make(map[string]EnrichedAttr, len(criticalAttrs))
	for _, id := range criticalAttrs {
		base := int64(0)
		cur := int64(0)
		if baseline != nil {
			base = baseline.Attrs[id]
		}
		if current != nil {
			cur = current.Attrs[id]
		}
		result[fmt.Sprintf("%d", id)] = EnrichedAttr{
			Name:     attrNames[id],
			Baseline: base,
			Current:  cur,
		}
	}
	return result
}

// TestResult describes the outcome of a SMART self-test.
type TestResult struct {
	Passed   bool   `json:"passed"`
	Status   string `json:"status"`
	Duration time.Duration `json:"duration"`
}

// SmartProgressCallback is invoked periodically during SMART self-tests to
// report estimated progress. percent is 0–100, elapsed is time since start.
type SmartProgressCallback func(percent float64, elapsed time.Duration, message string)

// smartctlCapabilities is the JSON structure for test polling times.
type smartctlCapabilities struct {
	ATASMARTData struct {
		Capabilities struct {
			Values []struct {
				Name   string `json:"name"`
			} `json:"values"`
		} `json:"capabilities"`
		SelfTest struct {
			PollingMinutes struct {
				Short    int `json:"short"`
				Extended int `json:"extended"`
			} `json:"polling_minutes"`
		} `json:"self_test"`
	} `json:"ata_smart_data"`
}

// smartctlAttrs is the JSON structure for SMART attribute tables.
type smartctlAttrs struct {
	ATASMARTAttributes struct {
		Table []struct {
			ID    int `json:"id"`
			Raw   struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
}

// smartctlSelfTestLog is the JSON structure for the self-test results log.
type smartctlSelfTestLog struct {
	ATASMARTSelfTestLog struct {
		Standard struct {
			Table []struct {
				Status struct {
					Passed bool   `json:"passed"`
					String string `json:"string"`
				} `json:"status"`
			} `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
}

// RunShortTest triggers a SMART short self-test and blocks until completion.
// If the context is cancelled, the hardware test is aborted via smartctl -X.
// The optional onProgress callback receives periodic heartbeat updates.
func RunShortTest(ctx context.Context, devicePath string, onProgress ...SmartProgressCallback) (*TestResult, error) {
	var cb SmartProgressCallback
	if len(onProgress) > 0 {
		cb = onProgress[0]
	}
	return runSelfTest(ctx, devicePath, "short", cb)
}

// RunLongTest triggers a SMART extended (long) self-test and blocks until completion.
// If the context is cancelled, the hardware test is aborted via smartctl -X.
// The optional onProgress callback receives periodic heartbeat updates.
func RunLongTest(ctx context.Context, devicePath string, onProgress ...SmartProgressCallback) (*TestResult, error) {
	var cb SmartProgressCallback
	if len(onProgress) > 0 {
		cb = onProgress[0]
	}
	return runSelfTest(ctx, devicePath, "long", cb)
}

func runSelfTest(ctx context.Context, devicePath, testType string, onProgress SmartProgressCallback) (*TestResult, error) {
	if !isValidDevicePath(devicePath) {
		return nil, fmt.Errorf("invalid device path: %q", devicePath)
	}

	pollInterval, err := getPollingTime(devicePath, testType)
	if err != nil {
		// Use conservative defaults if polling time unavailable.
		if testType == "short" {
			pollInterval = 2 * time.Minute
		} else {
			pollInterval = 120 * time.Minute
		}
	}

	// Start the self-test.
	startArg := fmt.Sprintf("--test=%s", testType)
	if _, err := runSmartctl(startArg, devicePath); err != nil {
		return nil, fmt.Errorf("starting %s self-test on %s: %w", testType, devicePath, err)
	}

	start := time.Now()

	// Register cleanup: abort the hardware test if context is cancelled.
	cleanup := context.AfterFunc(ctx, func() {
		abortSelfTest(devicePath)
	})
	defer cleanup()

	// Helper to emit heartbeat with estimated progress based on elapsed time.
	emitHeartbeat := func(msg string) {
		if onProgress == nil {
			return
		}
		elapsed := time.Since(start)
		// Estimate percent based on elapsed vs expected duration. Cap at 95%
		// since we can't know completion until smartctl confirms it.
		pct := float64(elapsed) / float64(pollInterval) * 100.0
		if pct > 95 {
			pct = 95
		}
		onProgress(pct, elapsed, msg)
	}

	// ── Initial wait phase with periodic heartbeats ──────────────────
	initialWait := time.Duration(float64(pollInterval) * 0.9)
	heartbeatTick := time.NewTicker(30 * time.Second)
	defer heartbeatTick.Stop()

	waitDeadline := time.After(initialWait)
	emitHeartbeat(fmt.Sprintf("SMART %s test running, estimated %s", testType, pollInterval.Round(time.Second)))
waitLoop:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-waitDeadline:
			emitHeartbeat(fmt.Sprintf("SMART %s initial wait complete, polling for result", testType))
			break waitLoop
		case <-heartbeatTick.C:
			emitHeartbeat(fmt.Sprintf("SMART %s test in progress", testType))
		}
	}

	// ── Polling phase ────────────────────────────────────────────────
	const checkInterval = 30 * time.Second
	for {
		done, passed, status, err := checkTestComplete(devicePath)
		if err != nil {
			return nil, fmt.Errorf("checking test status on %s: %w", devicePath, err)
		}

		if done {
			if onProgress != nil {
				onProgress(100, time.Since(start), fmt.Sprintf("SMART %s test complete", testType))
			}
			return &TestResult{
				Passed:   passed,
				Status:   status,
				Duration: time.Since(start),
			}, nil
		}

		emitHeartbeat(fmt.Sprintf("SMART %s test polling, awaiting completion", testType))

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(checkInterval):
		}
	}
}

// getPollingTime reads the estimated self-test polling time from smartctl.
func getPollingTime(devicePath, testType string) (time.Duration, error) {
	out, err := runSmartctl("-c", "--json", devicePath)
	if err != nil {
		return 0, err
	}

	var caps smartctlCapabilities
	if err := json.Unmarshal(out, &caps); err != nil {
		return 0, fmt.Errorf("parsing smartctl capabilities: %w", err)
	}

	var minutes int
	switch testType {
	case "short":
		minutes = caps.ATASMARTData.SelfTest.PollingMinutes.Short
	case "long":
		minutes = caps.ATASMARTData.SelfTest.PollingMinutes.Extended
	}

	if minutes <= 0 {
		return 0, fmt.Errorf("no polling time available for %s test", testType)
	}

	return time.Duration(minutes) * time.Minute, nil
}

// checkTestComplete examines the self-test log to determine if the most
// recent test has finished.
func checkTestComplete(devicePath string) (done, passed bool, status string, err error) {
	out, err := runSmartctl("-l", "selftest", "--json", devicePath)
	if err != nil {
		return false, false, "", err
	}

	var log smartctlSelfTestLog
	if err := json.Unmarshal(out, &log); err != nil {
		// Fall back to text-based check.
		return checkTestCompleteText(out)
	}

	table := log.ATASMARTSelfTestLog.Standard.Table
	if len(table) == 0 {
		return false, false, "no test entries", nil
	}

	latest := table[0]
	statusStr := latest.Status.String

	// "Self-test routine in progress" or similar indicates still running.
	if strings.Contains(strings.ToLower(statusStr), "in progress") ||
		strings.Contains(strings.ToLower(statusStr), "% remaining") {
		return false, false, statusStr, nil
	}

	return true, latest.Status.Passed, statusStr, nil
}

// checkTestCompleteText parses smartctl selftest log text output as fallback.
func checkTestCompleteText(out []byte) (done, passed bool, status string, err error) {
	text := string(out)

	if strings.Contains(text, "Self-test routine in progress") ||
		strings.Contains(text, "% of test remaining") {
		return false, false, "test in progress", nil
	}

	if strings.Contains(text, "Completed without error") {
		return true, true, "completed without error", nil
	}

	if strings.Contains(text, "Completed") {
		return true, false, "completed with errors", nil
	}

	// If no self-test log entries at all, treat as not started / done.
	return true, true, "no test entries found", nil
}

// abortSelfTest sends smartctl -X to cancel a running hardware self-test.
func abortSelfTest(devicePath string) {
	// Best-effort abort; errors are non-fatal since the context is already cancelled.
	exec.Command("smartctl", "-X", devicePath).Run()
}

// smartctlTemperature is the JSON structure for the temperature section.
type smartctlTemperature struct {
	Temperature struct {
		Current int `json:"current"`
	} `json:"temperature"`
}

// ReadTemperature retrieves the current drive temperature in °C via smartctl.
// Returns 0 if the temperature cannot be determined.
func ReadTemperature(devicePath string) (int, error) {
	if !isValidDevicePath(devicePath) {
		return 0, fmt.Errorf("invalid device path: %q", devicePath)
	}

	out, err := runSmartctl("-A", "--json", devicePath)
	if err != nil {
		return 0, fmt.Errorf("retrieving temperature for %s: %w", devicePath, err)
	}

	var t smartctlTemperature
	if err := json.Unmarshal(out, &t); err != nil {
		return 0, fmt.Errorf("parsing temperature JSON: %w", err)
	}

	return t.Temperature.Current, nil
}

// TakeSnapshot records the current raw values for critical SMART attributes.
func TakeSnapshot(devicePath string) (*SmartSnapshot, error) {
	if !isValidDevicePath(devicePath) {
		return nil, fmt.Errorf("invalid device path: %q", devicePath)
	}

	attrs, err := readSmartAttrs(devicePath)
	if err != nil {
		return nil, err
	}

	return &SmartSnapshot{
		Timestamp:  time.Now().UTC(),
		DevicePath: devicePath,
		Attrs:      attrs,
	}, nil
}

// ComputeDelta calculates the change in critical SMART attributes between
// a baseline and a post-test snapshot. Returns Degraded=true if any tracked
// attribute has increased (indicating potential drive failure).
func ComputeDelta(baseline, current *SmartSnapshot) *SmartDelta {
	delta := &SmartDelta{
		DevicePath: current.DevicePath,
		Deltas:     make(map[int]int64, len(criticalAttrs)),
	}

	for _, id := range criticalAttrs {
		baseVal := baseline.Attrs[id]
		curVal := current.Attrs[id]
		diff := curVal - baseVal
		delta.Deltas[id] = diff

		if diff > 0 {
			delta.Degraded = true
		}
	}

	return delta
}

// readSmartAttrs reads the SMART attribute table and returns raw values
// for critical attributes.
func readSmartAttrs(devicePath string) (map[int]int64, error) {
	out, err := runSmartctl("-A", "--json", devicePath)
	if err != nil {
		return nil, fmt.Errorf("reading SMART attributes for %s: %w", devicePath, err)
	}

	attrs, err := parseSmartAttrsJSON(out)
	if err != nil {
		// Fall back to text parsing.
		return parseSmartAttrsText(out)
	}
	return attrs, nil
}

// parseSmartAttrsJSON parses SMART attributes from smartctl JSON output.
func parseSmartAttrsJSON(data []byte) (map[int]int64, error) {
	var result smartctlAttrs
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	if len(result.ATASMARTAttributes.Table) == 0 {
		return nil, fmt.Errorf("no SMART attributes in JSON output")
	}

	criticalSet := make(map[int]bool, len(criticalAttrs))
	for _, id := range criticalAttrs {
		criticalSet[id] = true
	}

	attrs := make(map[int]int64, len(criticalAttrs))
	for _, entry := range result.ATASMARTAttributes.Table {
		if criticalSet[entry.ID] {
			attrs[entry.ID] = entry.Raw.Value
		}
	}

	return attrs, nil
}

// parseSmartAttrsText parses SMART attributes from smartctl text output.
// Each line has the format:
// ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE
func parseSmartAttrsText(data []byte) (map[int]int64, error) {
	criticalSet := make(map[int]bool, len(criticalAttrs))
	for _, id := range criticalAttrs {
		criticalSet[id] = true
	}

	attrs := make(map[int]int64, len(criticalAttrs))
	inTable := false

	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)

		// Detect the start of the attribute table.
		if strings.HasPrefix(trimmed, "ID#") {
			inTable = true
			continue
		}
		if !inTable || trimmed == "" {
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 10 {
			continue
		}

		id := 0
		fmt.Sscanf(fields[0], "%d", &id)
		if !criticalSet[id] {
			continue
		}

		var rawVal int64
		fmt.Sscanf(fields[9], "%d", &rawVal)
		attrs[id] = rawVal
	}

	return attrs, nil
}
