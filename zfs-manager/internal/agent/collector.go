package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// TelemetryPayload is the full telemetry frame transmitted to the Hub.
type TelemetryPayload struct {
	AgentID      string         `json:"agent_id"`
	Hostname     string         `json:"hostname"`
	Timestamp    time.Time      `json:"timestamp"`
	Pools        []PoolInfo     `json:"pools"`
	Datasets     []DatasetInfo  `json:"datasets"`
	Snapshots    []SnapshotInfo `json:"snapshots"`
	Capabilities Capabilities   `json:"capabilities"`
	ARC          *ARCStats      `json:"arc,omitempty"`
	IOStat       []PoolIOStat   `json:"iostat,omitempty"`
	LastEvent    *AgentEvent    `json:"last_event,omitempty"`
}

// AgentEvent is a single agent-side event surfaced to the hub for upstream
// notification dispatch. The hub deduplicates by ID, so the agent can keep
// the same event in subsequent telemetry frames without producing duplicate
// notifications.
type AgentEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// Collector periodically gathers ZFS state and caches it for telemetry.
type Collector struct {
	mu        sync.RWMutex
	agentID   string
	hostname  string
	engine    *Engine
	pools     []PoolInfo
	datasets  []DatasetInfo
	snapshots []SnapshotInfo
	arc       *ARCStats
	iostat    []PoolIOStat
	lastEvent *AgentEvent
	logger    *slog.Logger

	// flushCh signals the hub forwarder to send an immediate telemetry frame.
	flushCh chan struct{}
}

// NewCollector creates a telemetry Collector.
func NewCollector(agentID, hostname string, engine *Engine, logger *slog.Logger) *Collector {
	return &Collector{
		agentID:  agentID,
		hostname: hostname,
		engine:   engine,
		logger:   logger,
		flushCh:  make(chan struct{}, 1),
	}
}

// FlushCh returns a channel that signals when an immediate telemetry send is needed.
func (c *Collector) FlushCh() <-chan struct{} {
	return c.flushCh
}

// RequestFlush signals the hub forwarder to send telemetry immediately.
func (c *Collector) RequestFlush() {
	select {
	case c.flushCh <- struct{}{}:
	default:
	}
}

// EmitEvent stores a one-off agent event for inclusion in the next telemetry
// frame and immediately requests a flush so the hub sees it without waiting
// for the next periodic tick. The event remains in subsequent payloads — the
// hub deduplicates by ID, so callers don't need to clear it.
func (c *Collector) EmitEvent(event AgentEvent) {
	c.mu.Lock()
	c.lastEvent = &event
	c.mu.Unlock()
	c.RequestFlush()
}

// LastEvent returns the most recently emitted AgentEvent, or nil if none.
// Exposed for cross-package tests (e.g. the scheduler asserting that a
// scheduled task run produced the expected notification event).
func (c *Collector) LastEvent() *AgentEvent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastEvent == nil {
		return nil
	}
	evt := *c.lastEvent
	return &evt
}

// Refresh queries all ZFS state and updates the cache.
func (c *Collector) Refresh(ctx context.Context) {
	pools, err := c.engine.ListPools(ctx)
	if err != nil {
		c.logger.Error("failed to list pools", "error", err)
	}

	datasets, err := c.engine.ListDatasets(ctx)
	if err != nil {
		c.logger.Error("failed to list datasets", "error", err)
	}

	snapshots, err := c.engine.ListSnapshots(ctx)
	if err != nil {
		c.logger.Error("failed to list snapshots", "error", err)
	}

	// ARC stats are optional: the file is absent on non-Linux hosts and on
	// Linux before zfs.ko loads. ErrARCStatsUnavailable isn't worth logging
	// as an error — it's the expected state on systems without kstat.
	arc, err := c.engine.ReadARCStats(ctx)
	if err != nil && !errors.Is(err, ErrARCStatsUnavailable) {
		c.logger.Warn("failed to read arcstats", "error", err)
	}

	iostat, err := c.engine.SampleIOStat(ctx)
	if err != nil {
		c.logger.Warn("failed to sample iostat", "error", err)
	}

	c.mu.Lock()
	if pools != nil {
		c.pools = pools
	}
	if datasets != nil {
		c.datasets = datasets
	}
	if snapshots != nil {
		c.snapshots = snapshots
	}
	c.arc = arc
	if iostat != nil {
		c.iostat = iostat
	}
	c.mu.Unlock()
}

// GetPools returns cached pool info.
func (c *Collector) GetPools() []PoolInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pools
}

// GetDatasets returns cached dataset info.
func (c *Collector) GetDatasets() []DatasetInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.datasets
}

// GetSnapshots returns cached snapshot info.
func (c *Collector) GetSnapshots() []SnapshotInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshots
}

// GetARC returns the latest ARC stats snapshot, or nil if unavailable.
func (c *Collector) GetARC() *ARCStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.arc
}

// GetIOStat returns the latest per-pool iostat snapshot.
func (c *Collector) GetIOStat() []PoolIOStat {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.iostat
}

// Build assembles the current telemetry state into a payload.
func (c *Collector) Build() *TelemetryPayload {
	c.mu.RLock()
	defer c.mu.RUnlock()

	pools := c.pools
	if pools == nil {
		pools = []PoolInfo{}
	}
	datasets := c.datasets
	if datasets == nil {
		datasets = []DatasetInfo{}
	}
	snapshots := c.snapshots
	if snapshots == nil {
		snapshots = []SnapshotInfo{}
	}

	return &TelemetryPayload{
		AgentID:      c.agentID,
		Hostname:     c.hostname,
		Timestamp:    time.Now().UTC(),
		Pools:        pools,
		Datasets:     datasets,
		Snapshots:    snapshots,
		Capabilities: c.engine.Capabilities(),
		ARC:          c.arc,
		IOStat:       c.iostat,
		LastEvent:    c.lastEvent,
	}
}

// MarshalPayload serializes the current telemetry to JSON.
func (c *Collector) MarshalPayload() (json.RawMessage, error) {
	return json.Marshal(c.Build())
}

// StartCollectionLoop runs periodic telemetry collection.
func (c *Collector) StartCollectionLoop(ctx context.Context, interval time.Duration) {
	// Initial collection
	c.Refresh(ctx)
	c.logger.Info("initial telemetry collection complete")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Refresh(ctx)
		}
	}
}
