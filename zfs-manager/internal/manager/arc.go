package manager

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
)

// arcPayload is the hub-side view of an agent ARCStats snapshot. Kept local
// to avoid a cyclic import on the agent package; fields are a strict subset
// of agent.ARCStats and parse by JSON tag.
type arcPayload struct {
	Size       uint64 `json:"size"`
	TargetSize uint64 `json:"target_size"`
	MinSize    uint64 `json:"min_size"`
	MaxSize    uint64 `json:"max_size"`

	Hits     uint64  `json:"hits"`
	Misses   uint64  `json:"misses"`
	HitRatio float64 `json:"hit_ratio"`

	DemandDataHits       uint64 `json:"demand_data_hits"`
	DemandDataMisses     uint64 `json:"demand_data_misses"`
	DemandMetadataHits   uint64 `json:"demand_metadata_hits"`
	DemandMetadataMisses uint64 `json:"demand_metadata_misses"`
	PrefetchDataHits     uint64 `json:"prefetch_data_hits"`
	PrefetchDataMisses   uint64 `json:"prefetch_data_misses"`

	MRUSize uint64 `json:"mru_size"`
	MFUSize uint64 `json:"mfu_size"`

	L2Present  bool    `json:"l2_present"`
	L2Size     uint64  `json:"l2_size"`
	L2Asize    uint64  `json:"l2_asize"`
	L2Hits     uint64  `json:"l2_hits"`
	L2Misses   uint64  `json:"l2_misses"`
	L2HitRatio float64 `json:"l2_hit_ratio"`

	Timestamp       string   `json:"timestamp"`
	Recommendations []string `json:"recommendations,omitempty"`
}

// arcMetricRow is a single row in the Performance page's ARC metrics table.
// `Value` is the rendered string (bytes/percent) and `Raw` exposes the numeric
// value so the UI can sort or re-format. `Category` groups related metrics.
type arcMetricRow struct {
	Category string  `json:"category"`
	Metric   string  `json:"metric"`
	Value    string  `json:"value"`
	Raw      float64 `json:"raw"`
	Help     string  `json:"help,omitempty"`
}

// arcRecommendationRow mirrors the smart-table row shape for the
// recommendations table. Each entry gets a numeric ID so the table can use it
// as a stable row key.
type arcRecommendationRow struct {
	ID      int    `json:"id"`
	Message string `json:"message"`
}

// loadARCPayload reads the cached ARC field for the resolved agent. Returns
// nil if no agent is registered or the cache has no ARC entry (agent running
// on a non-ZFS host, or first telemetry frame hasn't landed yet).
func (s *Server) loadARCPayload(r *http.Request) *arcPayload {
	agentID := s.resolveAgentID(r)
	if agentID == "" {
		return nil
	}
	raw := s.aggregator.LatestField(agentID, "arc")
	if raw == nil || string(raw) == "null" {
		return nil
	}
	var p arcPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.logger.Warn("arc payload parse failed", "agent_id", agentID, "error", err)
		return nil
	}
	return &p
}

func (s *Server) handleARCMetrics(w http.ResponseWriter, r *http.Request) {
	p := s.loadARCPayload(r)
	if p == nil {
		// Empty array renders as "no data" in smart-table without an error state.
		addonutil.WriteJSON(w, http.StatusOK, []arcMetricRow{})
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, buildARCMetricRows(p))
}

func (s *Server) handleARCRecommendations(w http.ResponseWriter, r *http.Request) {
	p := s.loadARCPayload(r)
	if p == nil {
		addonutil.WriteJSON(w, http.StatusOK, []arcRecommendationRow{})
		return
	}
	rows := make([]arcRecommendationRow, len(p.Recommendations))
	for i, msg := range p.Recommendations {
		rows[i] = arcRecommendationRow{ID: i + 1, Message: msg}
	}
	addonutil.WriteJSON(w, http.StatusOK, rows)
}

// buildARCMetricRows flattens an ARC snapshot into the category → metric row
// shape the Performance page's smart-table renders. L2ARC rows are only
// emitted when a cache device is present so the table doesn't show a block
// of zeroes on pools without L2ARC.
func buildARCMetricRows(p *arcPayload) []arcMetricRow {
	rows := []arcMetricRow{
		{Category: "ARC", Metric: "Hit Ratio", Value: formatPercent(p.HitRatio), Raw: p.HitRatio,
			Help: "Fraction of reads served from memory. 90%+ is healthy."},
		{Category: "ARC", Metric: "Size", Value: humanBytes(p.Size), Raw: float64(p.Size),
			Help: "Current ARC memory footprint."},
		{Category: "ARC", Metric: "Target Size", Value: humanBytes(p.TargetSize), Raw: float64(p.TargetSize),
			Help: "Size ZFS is trying to maintain; adjusts dynamically within min/max bounds."},
		{Category: "ARC", Metric: "Max Size", Value: humanBytes(p.MaxSize), Raw: float64(p.MaxSize),
			Help: "Upper bound on ARC size (zfs_arc_max)."},
		{Category: "ARC", Metric: "Min Size", Value: humanBytes(p.MinSize), Raw: float64(p.MinSize),
			Help: "Lower bound on ARC size (zfs_arc_min)."},
		{Category: "ARC", Metric: "Hits", Value: formatCount(p.Hits), Raw: float64(p.Hits),
			Help: "Total reads served from ARC since boot."},
		{Category: "ARC", Metric: "Misses", Value: formatCount(p.Misses), Raw: float64(p.Misses),
			Help: "Total reads not found in ARC (went to disk or L2ARC)."},
		{Category: "ARC", Metric: "MRU Size", Value: humanBytes(p.MRUSize), Raw: float64(p.MRUSize),
			Help: "Most Recently Used: blocks read once recently."},
		{Category: "ARC", Metric: "MFU Size", Value: humanBytes(p.MFUSize), Raw: float64(p.MFUSize),
			Help: "Most Frequently Used: blocks read multiple times."},
		{Category: "Demand", Metric: "Data Hits", Value: formatCount(p.DemandDataHits), Raw: float64(p.DemandDataHits)},
		{Category: "Demand", Metric: "Data Misses", Value: formatCount(p.DemandDataMisses), Raw: float64(p.DemandDataMisses)},
		{Category: "Demand", Metric: "Metadata Hits", Value: formatCount(p.DemandMetadataHits), Raw: float64(p.DemandMetadataHits)},
		{Category: "Demand", Metric: "Metadata Misses", Value: formatCount(p.DemandMetadataMisses), Raw: float64(p.DemandMetadataMisses)},
		{Category: "Prefetch", Metric: "Data Hits", Value: formatCount(p.PrefetchDataHits), Raw: float64(p.PrefetchDataHits)},
		{Category: "Prefetch", Metric: "Data Misses", Value: formatCount(p.PrefetchDataMisses), Raw: float64(p.PrefetchDataMisses)},
	}

	if p.L2Present {
		rows = append(rows,
			arcMetricRow{Category: "L2ARC", Metric: "Hit Ratio", Value: formatPercent(p.L2HitRatio), Raw: p.L2HitRatio,
				Help: "Fraction of ARC misses served from the cache device."},
			arcMetricRow{Category: "L2ARC", Metric: "Size", Value: humanBytes(p.L2Size), Raw: float64(p.L2Size),
				Help: "Logical size of L2ARC data (before compression)."},
			arcMetricRow{Category: "L2ARC", Metric: "Allocated", Value: humanBytes(p.L2Asize), Raw: float64(p.L2Asize),
				Help: "Actual space used on the cache device."},
			arcMetricRow{Category: "L2ARC", Metric: "Hits", Value: formatCount(p.L2Hits), Raw: float64(p.L2Hits)},
			arcMetricRow{Category: "L2ARC", Metric: "Misses", Value: formatCount(p.L2Misses), Raw: float64(p.L2Misses)},
		)
	}
	return rows
}

// humanBytes mirrors the agent-side formatter so the hub-side display matches.
// Duplicated rather than imported because the hub package deliberately doesn't
// depend on the agent package.
func humanBytes(n uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
		TiB = 1024 * GiB
	)
	switch {
	case n >= TiB:
		return formatFloat(float64(n)/float64(TiB)) + " TiB"
	case n >= GiB:
		return formatFloat(float64(n)/float64(GiB)) + " GiB"
	case n >= MiB:
		return formatFloat(float64(n)/float64(MiB)) + " MiB"
	case n >= KiB:
		return formatFloat(float64(n)/float64(KiB)) + " KiB"
	default:
		return strconv.FormatUint(n, 10) + " B"
	}
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func formatPercent(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64) + "%"
}

// formatCount renders large counters with k/M/G suffixes. Raw byte sizes go
// through humanBytes instead; counters use SI so "1.2M hits" reads naturally.
func formatCount(n uint64) string {
	f := float64(n)
	switch {
	case n >= 1_000_000_000:
		return formatFloat(f/1e9) + "G"
	case n >= 1_000_000:
		return formatFloat(f/1e6) + "M"
	case n >= 1_000:
		return formatFloat(f/1e3) + "k"
	default:
		return strconv.FormatUint(n, 10)
	}
}
