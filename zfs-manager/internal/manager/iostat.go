package manager

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
)

// iostatPool is the hub-side view of an agent PoolIOStat sample. A strict
// subset of agent.PoolIOStat — kept local so the manager package doesn't
// import the agent package (cycles / binary bloat).
type iostatPool struct {
	Name       string       `json:"name"`
	Alloc      uint64       `json:"alloc"`
	Free       uint64       `json:"free"`
	ReadOps    uint64       `json:"read_ops"`
	WriteOps   uint64       `json:"write_ops"`
	ReadBytes  uint64       `json:"read_bytes"`
	WriteBytes uint64       `json:"write_bytes"`
	Vdevs      []iostatVdev `json:"vdevs,omitempty"`
	Timestamp  string       `json:"timestamp"`
}

type iostatVdev struct {
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	Alloc      uint64       `json:"alloc"`
	Free       uint64       `json:"free"`
	ReadOps    uint64       `json:"read_ops"`
	WriteOps   uint64       `json:"write_ops"`
	ReadBytes  uint64       `json:"read_bytes"`
	WriteBytes uint64       `json:"write_bytes"`
	Disks      []iostatDisk `json:"disks,omitempty"`
}

type iostatDisk struct {
	Name       string `json:"name"`
	ReadOps    uint64 `json:"read_ops"`
	WriteOps   uint64 `json:"write_ops"`
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
}

// iostatRow is one flattened row rendered by the Performance page's I/O table.
// Each row represents a pool, vdev, or leaf disk; `Scope` groups them in the
// UI and `Parent` lets nested rows show their hierarchy without deeply nested
// table widgets.
type iostatRow struct {
	Scope      string  `json:"scope"` // "pool" | "vdev" | "disk"
	Name       string  `json:"name"`
	Parent     string  `json:"parent,omitempty"`
	VdevType   string  `json:"vdev_type,omitempty"`
	ReadOps    float64 `json:"read_ops_per_s"`
	WriteOps   float64 `json:"write_ops_per_s"`
	ReadBytes  float64 `json:"read_bytes_per_s"`
	WriteBytes float64 `json:"write_bytes_per_s"`
	// Cumulative counters kept for users who want totals instead of rates.
	TotalReadOps    uint64 `json:"total_read_ops"`
	TotalWriteOps   uint64 `json:"total_write_ops"`
	TotalReadBytes  uint64 `json:"total_read_bytes"`
	TotalWriteBytes uint64 `json:"total_write_bytes"`
	// Alloc/Free only meaningful at pool + vdev scope; zero for leaves.
	Alloc uint64 `json:"alloc,omitempty"`
	Free  uint64 `json:"free,omitempty"`
}

// handleIOStat returns the raw iostat array (cumulative counters) for the
// resolved agent. Kept for clients that want to compute their own rates or
// inspect raw values; the Performance page uses /api/iostat/rows instead.
func (s *Server) handleIOStat(w http.ResponseWriter, r *http.Request) {
	agentID := s.resolveAgentID(r)
	if agentID != "" {
		if data := s.aggregator.LatestField(agentID, "iostat"); data != nil && string(data) != "null" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
	}
	// Empty array so smart-table shows "no data" rather than a request error.
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("[]"))
}

// handleIOStatRows flattens the cached iostat sample into smart-table rows and
// computes per-second rates by diffing against the previous frame. When no
// previous frame exists (first telemetry after boot) rates are reported as
// zero — totals still reflect the cumulative values.
func (s *Server) handleIOStatRows(w http.ResponseWriter, r *http.Request) {
	agentID := s.resolveAgentID(r)
	if agentID == "" {
		addonutil.WriteJSON(w, http.StatusOK, []iostatRow{})
		return
	}
	currRaw := s.aggregator.LatestField(agentID, "iostat")
	if currRaw == nil || string(currRaw) == "null" {
		addonutil.WriteJSON(w, http.StatusOK, []iostatRow{})
		return
	}
	var curr []iostatPool
	if err := json.Unmarshal(currRaw, &curr); err != nil {
		s.logger.Warn("iostat current parse failed", "agent_id", agentID, "error", err)
		addonutil.WriteJSON(w, http.StatusOK, []iostatRow{})
		return
	}

	var prev []iostatPool
	if prevRaw := s.aggregator.PreviousIOStat(agentID); prevRaw != nil {
		// A parse failure here degrades gracefully to zero-rate rows rather
		// than an error — the client can still see the cumulative counters.
		_ = json.Unmarshal(prevRaw, &prev)
	}
	addonutil.WriteJSON(w, http.StatusOK, buildIOStatRows(curr, prev))
}

// buildIOStatRows is the pure transformation used by handleIOStatRows; split
// out so the rate math can be exercised by unit tests without an HTTP harness.
func buildIOStatRows(curr, prev []iostatPool) []iostatRow {
	prevByPool := indexPools(prev)
	rows := make([]iostatRow, 0, len(curr)*4)

	for _, p := range curr {
		pPrev, havePrev := prevByPool[p.Name]
		dt := deltaSeconds(p.Timestamp, pPrev.Timestamp, havePrev)

		rows = append(rows, iostatRow{
			Scope:           "pool",
			Name:            p.Name,
			ReadOps:         rate(p.ReadOps, pPrev.ReadOps, dt, havePrev),
			WriteOps:        rate(p.WriteOps, pPrev.WriteOps, dt, havePrev),
			ReadBytes:       rate(p.ReadBytes, pPrev.ReadBytes, dt, havePrev),
			WriteBytes:      rate(p.WriteBytes, pPrev.WriteBytes, dt, havePrev),
			TotalReadOps:    p.ReadOps,
			TotalWriteOps:   p.WriteOps,
			TotalReadBytes:  p.ReadBytes,
			TotalWriteBytes: p.WriteBytes,
			Alloc:           p.Alloc,
			Free:            p.Free,
		})

		prevVdevs := indexVdevs(pPrev.Vdevs)
		for _, v := range p.Vdevs {
			vPrev, haveVdev := prevVdevs[v.Name]
			vHave := havePrev && haveVdev
			rows = append(rows, iostatRow{
				Scope:           "vdev",
				Name:            v.Name,
				Parent:          p.Name,
				VdevType:        v.Type,
				ReadOps:         rate(v.ReadOps, vPrev.ReadOps, dt, vHave),
				WriteOps:        rate(v.WriteOps, vPrev.WriteOps, dt, vHave),
				ReadBytes:       rate(v.ReadBytes, vPrev.ReadBytes, dt, vHave),
				WriteBytes:      rate(v.WriteBytes, vPrev.WriteBytes, dt, vHave),
				TotalReadOps:    v.ReadOps,
				TotalWriteOps:   v.WriteOps,
				TotalReadBytes:  v.ReadBytes,
				TotalWriteBytes: v.WriteBytes,
				Alloc:           v.Alloc,
				Free:            v.Free,
			})

			prevDisks := indexDisks(vPrev.Disks)
			for _, d := range v.Disks {
				dPrev, haveDisk := prevDisks[d.Name]
				dHave := vHave && haveDisk
				rows = append(rows, iostatRow{
					Scope:           "disk",
					Name:            d.Name,
					Parent:          v.Name,
					ReadOps:         rate(d.ReadOps, dPrev.ReadOps, dt, dHave),
					WriteOps:        rate(d.WriteOps, dPrev.WriteOps, dt, dHave),
					ReadBytes:       rate(d.ReadBytes, dPrev.ReadBytes, dt, dHave),
					WriteBytes:      rate(d.WriteBytes, dPrev.WriteBytes, dt, dHave),
					TotalReadOps:    d.ReadOps,
					TotalWriteOps:   d.WriteOps,
					TotalReadBytes:  d.ReadBytes,
					TotalWriteBytes: d.WriteBytes,
				})
			}
		}
	}
	return rows
}

// rate returns the per-second delta between two cumulative counters. Guards
// against counter decreases (pool export/re-import resets to zero) and
// zero/negative deltas by falling back to zero instead of returning a bogus
// "spike" number that would mislead operators.
func rate(curr, prev uint64, dt float64, havePrev bool) float64 {
	if !havePrev || dt <= 0 || curr < prev {
		return 0
	}
	return float64(curr-prev) / dt
}

// deltaSeconds returns the wall-clock gap between two RFC3339 iostat
// timestamps. Falls back to 0 (→ rate() returns 0) if either timestamp is
// missing or unparsable — on fresh starts and on agent-side clock skew the
// hub would rather render zeros than a wildly wrong rate.
func deltaSeconds(currTS, prevTS string, havePrev bool) float64 {
	if !havePrev {
		return 0
	}
	c, err := time.Parse(time.RFC3339, currTS)
	if err != nil {
		return 0
	}
	p, err := time.Parse(time.RFC3339, prevTS)
	if err != nil {
		return 0
	}
	d := c.Sub(p).Seconds()
	if d <= 0 {
		return 0
	}
	return d
}

func indexPools(pools []iostatPool) map[string]iostatPool {
	out := make(map[string]iostatPool, len(pools))
	for _, p := range pools {
		out[p.Name] = p
	}
	return out
}

func indexVdevs(vdevs []iostatVdev) map[string]iostatVdev {
	out := make(map[string]iostatVdev, len(vdevs))
	for _, v := range vdevs {
		out[v.Name] = v
	}
	return out
}

func indexDisks(disks []iostatDisk) map[string]iostatDisk {
	out := make(map[string]iostatDisk, len(disks))
	for _, d := range disks {
		out[d.Name] = d
	}
	return out
}
