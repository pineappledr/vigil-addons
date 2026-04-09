package agent

import (
	"context"
	"encoding/json"
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
