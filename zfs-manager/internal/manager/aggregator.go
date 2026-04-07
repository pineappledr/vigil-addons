package manager

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/pineappledr/vigil-addons/shared/vigilclient"
)

// Aggregator caches per-agent telemetry and forwards frames upstream to Vigil.
type Aggregator struct {
	mu       sync.RWMutex
	cache    map[string]json.RawMessage // agentID → latest telemetry payload
	upstream chan []byte
	vigil    *vigilclient.TelemetryClient
	logger   *slog.Logger
}

// NewAggregator creates an Aggregator.
func NewAggregator(upstream chan []byte, logger *slog.Logger) *Aggregator {
	return &Aggregator{
		cache:    make(map[string]json.RawMessage),
		upstream: upstream,
		logger:   logger,
	}
}

// SetTelemetryClient wires the upstream Vigil telemetry client.
func (a *Aggregator) SetTelemetryClient(c *vigilclient.TelemetryClient) {
	a.vigil = c
}

// Ingest stores a telemetry payload from an agent and forwards it upstream.
func (a *Aggregator) Ingest(agentID string, payload json.RawMessage) {
	a.mu.Lock()
	a.cache[agentID] = payload
	a.mu.Unlock()

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
