package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// ARCStatsPath is the kstat file the ZFS kernel module exposes on Linux.
// Absent on non-Linux (Darwin dev hosts, BSDs) and on Linux without zfs.ko loaded.
const ARCStatsPath = "/proc/spl/kstat/zfs/arcstats"

// ErrARCStatsUnavailable is returned by Engine.ReadARCStats when the host
// cannot expose ARC kstats — typically because /proc/spl/kstat/zfs/arcstats
// does not exist (no ZFS kernel module) or the agent lacks read permission.
var ErrARCStatsUnavailable = errors.New("arcstats not available on this host")

// ARCStats is a snapshot of the ZFS ARC (and L2ARC, if present) performance
// counters, plus a small set of plain-English recommendations derived from
// them. Consumed by the hub's /api/arc proxy and rendered on the Performance
// page in the UI.
type ARCStats struct {
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

// ARCStatsAvailable reports whether the host exposes the ARC kstat file.
// Used at engine startup to set the Capabilities.ARCStats flag.
func ARCStatsAvailable() bool {
	_, err := os.Stat(ARCStatsPath)
	return err == nil
}

// ReadARCStats parses /proc/spl/kstat/zfs/arcstats and returns a populated
// snapshot. Returns ErrARCStatsUnavailable if the file is absent so callers
// can surface a clean "not supported on this host" message.
func (e *Engine) ReadARCStats(ctx context.Context) (*ARCStats, error) {
	if _, err := os.Stat(ARCStatsPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrARCStatsUnavailable
		}
		return nil, err
	}

	// #nosec G304 -- ARCStatsPath is a fixed kstat path, not user input.
	data, err := os.ReadFile(ARCStatsPath)
	if err != nil {
		return nil, err
	}
	stats, err := parseARCStats(data)
	if err != nil {
		return nil, err
	}
	stats.Timestamp = time.Now().UTC().Format(time.RFC3339)
	stats.Recommendations = arcRecommendations(stats)
	return stats, nil
}

// parseARCStats decodes the kstat text format. The file begins with two
// header lines (kstat version + "name type data" column names); all later
// lines are whitespace-separated triples. Unknown keys are ignored so future
// ZFS versions adding counters don't break the parser.
func parseARCStats(data []byte) (*ARCStats, error) {
	s := &ARCStats{}
	raw := map[string]uint64{}

	sc := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for sc.Scan() {
		line++
		// Skip the two header lines.
		if line <= 2 {
			continue
		}
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) < 3 {
			continue
		}
		v, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			continue
		}
		raw[fields[0]] = v
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("arcstats: no counters parsed")
	}

	s.Size = raw["size"]
	s.TargetSize = raw["c"]
	s.MinSize = raw["c_min"]
	s.MaxSize = raw["c_max"]

	s.Hits = raw["hits"]
	s.Misses = raw["misses"]
	if total := s.Hits + s.Misses; total > 0 {
		s.HitRatio = float64(s.Hits) / float64(total) * 100
	}

	s.DemandDataHits = raw["demand_data_hits"]
	s.DemandDataMisses = raw["demand_data_misses"]
	s.DemandMetadataHits = raw["demand_metadata_hits"]
	s.DemandMetadataMisses = raw["demand_metadata_misses"]
	s.PrefetchDataHits = raw["prefetch_data_hits"]
	s.PrefetchDataMisses = raw["prefetch_data_misses"]

	s.MRUSize = raw["mru_size"]
	s.MFUSize = raw["mfu_size"]

	// L2ARC is present when the kernel has ever seen an L2ARC device; we treat
	// a non-zero l2_size as the signal. l2_hits + l2_misses alone aren't
	// sufficient because a freshly-added cache device reports both as 0.
	s.L2Size = raw["l2_size"]
	s.L2Asize = raw["l2_asize"]
	s.L2Hits = raw["l2_hits"]
	s.L2Misses = raw["l2_misses"]
	s.L2Present = s.L2Size > 0 || s.L2Hits+s.L2Misses > 0
	if total := s.L2Hits + s.L2Misses; total > 0 {
		s.L2HitRatio = float64(s.L2Hits) / float64(total) * 100
	}

	return s, nil
}

// arcRecommendations derives plain-English suggestions from the snapshot.
// Thresholds are intentionally conservative — the point is to flag the
// obvious misconfigurations (grossly undersized ARC, useless L2ARC) without
// nagging on healthy systems.
func arcRecommendations(s *ARCStats) []string {
	var recs []string
	total := s.Hits + s.Misses
	if total > 10000 {
		switch {
		case s.HitRatio < 70:
			recs = append(recs, "ARC hit ratio is "+formatPercent(s.HitRatio)+
				" — the ARC is significantly undersized for this workload. Adding more RAM will give the biggest performance boost.")
		case s.HitRatio < 90:
			recs = append(recs, "ARC hit ratio is "+formatPercent(s.HitRatio)+
				" — healthy systems typically run at 90%+. Consider adding more RAM if performance matters here.")
		}
	}

	// Flag an L2ARC that almost never serves a read. A freshly-added cache
	// device gets a grace period (l2_hits+l2_misses > 100k) before we call it
	// out, so we don't nag during the warmup window.
	if s.L2Present {
		l2total := s.L2Hits + s.L2Misses
		if l2total > 100000 && s.L2HitRatio < 5 {
			recs = append(recs, "L2ARC hit ratio is "+formatPercent(s.L2HitRatio)+
				" — the cache device is rarely serving reads. It may be providing little value; consider removing it to free the SSD for other use.")
		}
	}

	// ARC can't grow near its cap — suggests c_max is set too low for the host.
	if s.MaxSize > 0 && s.TargetSize > 0 {
		headroom := float64(s.MaxSize-s.TargetSize) / float64(s.MaxSize)
		if s.HitRatio < 90 && headroom > 0.5 && total > 10000 {
			recs = append(recs, "ARC target size is well below its cap ("+
				humanBytes(s.TargetSize)+" of "+humanBytes(s.MaxSize)+
				"). If you have spare RAM, raising zfs_arc_max can improve hit ratio.")
		}
	}

	return recs
}

func formatPercent(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64) + "%"
}
