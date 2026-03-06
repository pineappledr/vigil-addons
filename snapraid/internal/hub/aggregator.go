package hub

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// AggregatedFrame wraps an Agent's telemetry with its identity for upstream transmission.
type AggregatedFrame struct {
	AgentID   string          `json:"agent_id"`
	Hostname  string          `json:"hostname"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// Aggregator multiplexes inbound WebSocket telemetry from multiple Agents
// and pushes wrapped frames upstream to the Vigil Server.
type Aggregator struct {
	mu       sync.RWMutex
	registry *Registry
	upstream chan<- []byte
	logger   *slog.Logger
}

// NewAggregator creates a telemetry Aggregator. Upstream frames are sent
// to the provided channel for the Vigil Client to transmit.
func NewAggregator(registry *Registry, upstream chan<- []byte, logger *slog.Logger) *Aggregator {
	return &Aggregator{
		registry: registry,
		upstream: upstream,
		logger:   logger,
	}
}

// IngestAgentFrame receives a raw telemetry payload from an Agent,
// wraps it with Agent metadata, and forwards it upstream.
func (a *Aggregator) IngestAgentFrame(agentID string, raw []byte) {
	entry := a.registry.Get(agentID)
	if entry == nil {
		a.logger.Warn("telemetry from unknown agent", "agent_id", agentID)
		return
	}

	a.registry.Touch(agentID)

	frame := AggregatedFrame{
		AgentID:   agentID,
		Hostname:  entry.Hostname,
		Timestamp: time.Now().UTC(),
		Payload:   raw,
	}

	data, err := json.Marshal(frame)
	if err != nil {
		a.logger.Error("failed to marshal aggregated frame", "agent_id", agentID, "error", err)
		return
	}

	select {
	case a.upstream <- data:
	default:
		a.logger.Warn("upstream channel full, dropping aggregated frame", "agent_id", agentID)
	}
}

// IngestLogStream forwards a real-time log stream frame from an Agent upstream.
func (a *Aggregator) IngestLogStream(agentID string, raw []byte) {
	frame := AggregatedFrame{
		AgentID:   agentID,
		Timestamp: time.Now().UTC(),
		Payload:   raw,
	}

	data, err := json.Marshal(frame)
	if err != nil {
		a.logger.Error("failed to marshal log stream frame", "agent_id", agentID, "error", err)
		return
	}

	select {
	case a.upstream <- data:
	default:
		a.logger.Warn("upstream channel full, dropping log frame", "agent_id", agentID)
	}
}
