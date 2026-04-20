package manager

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pineappledr/vigil-addons/shared/vigilclient"
)

// Aggregator caches per-agent telemetry and forwards frames upstream to Vigil.
//
// In addition to the cache + forwarding role, it inspects each incoming frame
// for state transitions (resilver completion, pool expansion) and forwards
// agent-emitted one-off events (drive replacement started) as typed
// notifications upstream. The agent itself only ever ships state — the hub is
// responsible for diff-based notification semantics.
type Aggregator struct {
	mu       sync.RWMutex
	registry *Registry
	cache    map[string]json.RawMessage // agentID → latest telemetry payload
	upstream chan []byte
	vigil    *vigilclient.TelemetryClient
	logger   *slog.Logger

	// Per-agent transition state. Snapshots the last-seen scrub status and
	// data vdev count per pool so we can detect "was resilvering, no longer
	// resilvering" and "vdev count went up" between successive frames.
	lastPoolStates map[string]map[string]trackedPool // agentID → poolName → state
	lastEventIDs   map[string]string                 // agentID → last forwarded LastEvent.ID

	// prevIOStat holds the iostat field from the frame immediately preceding
	// the one in `cache`. Rates are derived by diffing counters between the
	// current cached sample and this one — keeping it at the aggregator so
	// shifting is atomic with a new Ingest overwriting the cache.
	prevIOStat map[string]json.RawMessage // agentID → prior iostat snapshot
}

// trackedPool is the per-pool transition state retained between frames.
type trackedPool struct {
	ScrubStatus  string
	NumDataVdevs int
}

// poolEvent describes a pool-level transition that should fan out as a
// notification. Returned by detectPoolTransitions so the diff logic stays
// pure and unit-testable.
type poolEvent struct {
	EventType string
	Severity  string
	PoolName  string
	Message   string
}

// Minimal subset of the agent's TelemetryPayload that we need to inspect for
// transition detection. Avoids importing the agent package and keeps this
// resilient against unrelated payload field additions.
type telemetryMin struct {
	Pools     []poolMin      `json:"pools"`
	LastEvent *agentEventMin `json:"last_event,omitempty"`
}

type poolMin struct {
	Name        string    `json:"name"`
	ScrubStatus string    `json:"scrub_status"`
	Vdevs       []vdevMin `json:"vdevs,omitempty"`
}

type vdevMin struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type agentEventMin struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// NewAggregator creates an Aggregator. The registry is used to resolve agent
// hostnames when emitting notifications upstream.
func NewAggregator(registry *Registry, upstream chan []byte, logger *slog.Logger) *Aggregator {
	return &Aggregator{
		registry:       registry,
		cache:          make(map[string]json.RawMessage),
		upstream:       upstream,
		logger:         logger,
		lastPoolStates: make(map[string]map[string]trackedPool),
		lastEventIDs:   make(map[string]string),
		prevIOStat:     make(map[string]json.RawMessage),
	}
}

// SetTelemetryClient wires the upstream Vigil telemetry client.
func (a *Aggregator) SetTelemetryClient(c *vigilclient.TelemetryClient) {
	a.mu.Lock()
	a.vigil = c
	a.mu.Unlock()
}

// Ingest stores a telemetry payload from an agent and forwards it upstream.
// Before forwarding, the payload is inspected for state transitions and any
// detected events are emitted as typed notifications.
func (a *Aggregator) Ingest(agentID string, payload json.RawMessage) {
	a.mu.Lock()
	// Before overwriting the cached frame, extract its iostat field and keep
	// it as the "previous" sample so rate diffs have something to subtract
	// against on the next render. If the previous cache entry has no iostat
	// (e.g. agent running on a non-ZFS host), we just drop the placeholder.
	if prev, ok := a.cache[agentID]; ok {
		if iostat := extractField(prev, "iostat"); iostat != nil {
			a.prevIOStat[agentID] = iostat
		}
	}
	a.cache[agentID] = payload
	a.mu.Unlock()

	a.evaluateForNotifications(agentID, payload)

	// Wrap with agent_id and forward upstream as a telemetry frame.
	type frame struct {
		AgentID string          `json:"agent_id"`
		Payload json.RawMessage `json:"payload"`
	}
	data, err := json.Marshal(frame{AgentID: agentID, Payload: payload})
	if err != nil {
		a.logger.Error("failed to marshal upstream frame", "error", err)
		return
	}

	select {
	case a.upstream <- data:
	default:
		a.logger.Warn("upstream channel full, dropping telemetry frame", "agent_id", agentID)
	}
}

// evaluateForNotifications diffs the incoming payload against the last
// snapshot for this agent and emits notifications for any detected transitions
// (pool state changes) and one-off agent events.
func (a *Aggregator) evaluateForNotifications(agentID string, payload json.RawMessage) {
	a.mu.RLock()
	tc := a.vigil
	a.mu.RUnlock()
	if tc == nil {
		return
	}

	var t telemetryMin
	if err := json.Unmarshal(payload, &t); err != nil {
		a.logger.Debug("notification eval: failed to parse payload", "agent_id", agentID, "error", err)
		return
	}

	// Build current snapshot.
	curr := make(map[string]trackedPool, len(t.Pools))
	for _, p := range t.Pools {
		curr[p.Name] = trackedPool{
			ScrubStatus:  p.ScrubStatus,
			NumDataVdevs: countDataVdevs(p.Vdevs),
		}
	}

	a.mu.Lock()
	prev := a.lastPoolStates[agentID]
	a.lastPoolStates[agentID] = curr
	a.mu.Unlock()

	// Pool-level transitions (skipped on first frame to avoid baseline noise).
	if prev != nil {
		for _, evt := range detectPoolTransitions(prev, curr) {
			a.emitNotification(tc, agentID, evt.EventType, evt.Severity, evt.Message)
		}
	}

	// Agent-emitted one-off event (e.g. drive_replacement_started).
	a.evaluateAgentEvent(tc, agentID, t.LastEvent)
}

// evaluateAgentEvent forwards an agent-emitted event upstream as a typed
// notification, deduplicating by event ID so the same event in successive
// telemetry frames only fires once.
func (a *Aggregator) evaluateAgentEvent(tc *vigilclient.TelemetryClient, agentID string, evt *agentEventMin) {
	if evt == nil || evt.ID == "" {
		return
	}

	a.mu.Lock()
	if a.lastEventIDs[agentID] == evt.ID {
		a.mu.Unlock()
		return
	}
	a.lastEventIDs[agentID] = evt.ID
	a.mu.Unlock()

	severity := evt.Severity
	if severity == "" {
		severity = "info"
	}
	a.emitNotification(tc, agentID, evt.Type, severity, evt.Message)
}

// detectPoolTransitions diffs two pool snapshots and returns the events that
// should be emitted upstream. Pure function — no I/O — so the transition
// rules are exercised directly by unit tests.
//
// Detected transitions:
//   - resilver_completed: prev.ScrubStatus == "resilvering" and curr is not
//   - scrub_completed: prev was `in_progress[…]` and curr is `completed`
//   - pool_expansion_completed: curr.NumDataVdevs > prev.NumDataVdevs
func detectPoolTransitions(prev, curr map[string]trackedPool) []poolEvent {
	var events []poolEvent
	for name, c := range curr {
		p, ok := prev[name]
		if !ok {
			// New pool — establish baseline only.
			continue
		}
		if isResilvering(p.ScrubStatus) && !isResilvering(c.ScrubStatus) {
			events = append(events, poolEvent{
				EventType: "resilver_completed",
				Severity:  "info",
				PoolName:  name,
				Message:   fmt.Sprintf("Resilver completed on pool %s", name),
			})
		}
		if isScrubRunning(p.ScrubStatus) && c.ScrubStatus == "completed" {
			events = append(events, poolEvent{
				EventType: "scrub_completed",
				Severity:  "info",
				PoolName:  name,
				Message:   fmt.Sprintf("Scrub completed on pool %s", name),
			})
		}
		if c.NumDataVdevs > p.NumDataVdevs {
			added := c.NumDataVdevs - p.NumDataVdevs
			noun := "vdev"
			if added != 1 {
				noun = "vdevs"
			}
			events = append(events, poolEvent{
				EventType: "pool_expansion_completed",
				Severity:  "info",
				PoolName:  name,
				Message:   fmt.Sprintf("Pool %s expanded: %d new data %s (%d → %d)", name, added, noun, p.NumDataVdevs, c.NumDataVdevs),
			})
		}
	}
	return events
}

// countDataVdevs counts data vdevs in a pool, ignoring cache/log/spare/special
// vdevs which legitimately coexist with any data layout.
func countDataVdevs(vdevs []vdevMin) int {
	n := 0
	for _, v := range vdevs {
		switch v.Type {
		case "cache", "log", "spare", "special":
			continue
		}
		n++
	}
	return n
}

// isResilvering reports whether the given scrub_status string indicates a
// resilver is currently running. The agent reports "resilvering" with no
// progress decoration today, but tolerate a "resilvering (X% done)" form too
// in case the parser is extended later.
func isResilvering(status string) bool {
	return strings.HasPrefix(status, "resilvering")
}

// isScrubRunning reports whether the given scrub_status indicates a scrub in
// progress. The agent emits "in_progress" or "in_progress (X% done)", plus
// the transient "paused" state — both count as running for the purpose of
// detecting a completion transition.
func isScrubRunning(status string) bool {
	return strings.HasPrefix(status, "in_progress") || status == "paused"
}

// EmitLogLine forwards a real-time log line upstream as a typed "log" frame
// so it surfaces on the dashboard Logs page via SSE (event: log). Best-effort:
// a disconnected upstream drops the line rather than blocking the ingest path.
func (a *Aggregator) EmitLogLine(payload map[string]string) {
	a.mu.RLock()
	tc := a.vigil
	a.mu.RUnlock()

	if tc == nil {
		return
	}

	if err := tc.Send("log", payload); err != nil {
		a.logger.Debug("failed to send log line upstream", "error", err)
	}
}

// emitNotification looks up the agent hostname from the registry and sends a
// typed notification frame upstream.
func (a *Aggregator) emitNotification(tc *vigilclient.TelemetryClient, agentID, eventType, severity, message string) {
	hostname := agentID
	if entry := a.registry.Get(agentID); entry != nil && entry.Hostname != "" {
		hostname = entry.Hostname
	}

	n := vigilclient.NotificationPayload{
		EventType: eventType,
		Severity:  severity,
		Source:    "zfs-manager",
		Host:      hostname,
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	if err := tc.Send("notification", n); err != nil {
		a.logger.Warn("failed to transmit notification upstream",
			"event_type", eventType,
			"agent_id", agentID,
			"error", err,
		)
		return
	}

	a.logger.Info("notification emitted",
		"event_type", eventType,
		"severity", severity,
		"agent_id", agentID,
	)
}

// Latest returns the cached telemetry for an agent, or nil.
func (a *Aggregator) Latest(agentID string) json.RawMessage {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cache[agentID]
}

// LatestField extracts a top-level JSON field from the cached telemetry for an agent.
// Returns nil if no cache entry or field is absent.
func (a *Aggregator) LatestField(agentID, field string) json.RawMessage {
	a.mu.RLock()
	raw := a.cache[agentID]
	a.mu.RUnlock()
	return extractField(raw, field)
}

// PreviousIOStat returns the iostat field from the telemetry frame that was
// cached immediately before the current one, or nil if this is the first
// frame the hub has seen for the agent. The hub uses the difference between
// this and the latest sample to compute rates without requiring the agent to
// remember state between polls.
func (a *Aggregator) PreviousIOStat(agentID string) json.RawMessage {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.prevIOStat[agentID]
}

// extractField pulls a top-level key out of a JSON object payload without
// unmarshalling anything inside it. Kept package-private because callers
// should route through Latest / LatestField / PreviousIOStat.
func extractField(raw json.RawMessage, field string) json.RawMessage {
	if raw == nil {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	return obj[field]
}

// LatestAll returns all cached telemetry payloads keyed by agent ID.
func (a *Aggregator) LatestAll() map[string]json.RawMessage {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]json.RawMessage, len(a.cache))
	for k, v := range a.cache {
		out[k] = v
	}
	return out
}
