package agent

import (
	"strings"
	"testing"
)

// sampleArcstats is a trimmed but realistic kstat payload. Includes the two
// header lines the parser must skip and a mix of the counters we consume.
const sampleArcstats = `7 1 0x01 112 26880 1234567890 9876543210
name                            type data
hits                            4    9000
misses                          4    1000
demand_data_hits                4    4000
demand_data_misses              4    400
demand_metadata_hits            4    3500
demand_metadata_misses          4    500
prefetch_data_hits              4    1000
prefetch_data_misses            4    50
prefetch_metadata_hits          4    500
prefetch_metadata_misses        4    50
mru_size                        4    536870912
mfu_size                        4    1073741824
size                            4    2147483648
c                               4    2147483648
c_min                           4    268435456
c_max                           4    4294967296
l2_size                         4    10737418240
l2_asize                        4    5368709120
l2_hits                         4    800
l2_misses                       4    200
l2_writes_sent                  4    12345
`

func TestParseARCStats_Basic(t *testing.T) {
	s, err := parseARCStats([]byte(sampleArcstats))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if s.Hits != 9000 || s.Misses != 1000 {
		t.Fatalf("hits/misses: got %d/%d", s.Hits, s.Misses)
	}
	if s.HitRatio < 89.9 || s.HitRatio > 90.1 {
		t.Fatalf("hit ratio: got %.2f, want ~90", s.HitRatio)
	}
	if s.Size != 2147483648 {
		t.Fatalf("size: got %d", s.Size)
	}
	if s.TargetSize != 2147483648 {
		t.Fatalf("target size: got %d", s.TargetSize)
	}
	if s.MaxSize != 4294967296 || s.MinSize != 268435456 {
		t.Fatalf("min/max: got %d / %d", s.MinSize, s.MaxSize)
	}
	if s.MRUSize != 536870912 || s.MFUSize != 1073741824 {
		t.Fatalf("mru/mfu: got %d / %d", s.MRUSize, s.MFUSize)
	}
}

func TestParseARCStats_L2ARCDetected(t *testing.T) {
	s, err := parseARCStats([]byte(sampleArcstats))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !s.L2Present {
		t.Fatal("expected l2_present=true when l2_size > 0")
	}
	if s.L2Size != 10737418240 || s.L2Asize != 5368709120 {
		t.Fatalf("l2 sizes: got %d / %d", s.L2Size, s.L2Asize)
	}
	if s.L2HitRatio < 79.9 || s.L2HitRatio > 80.1 {
		t.Fatalf("l2 hit ratio: got %.2f, want ~80", s.L2HitRatio)
	}
}

func TestParseARCStats_NoL2ARC(t *testing.T) {
	// Same payload minus the l2_* lines → L2Present must be false.
	filtered := []string{}
	for _, line := range strings.Split(sampleArcstats, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "l2_") {
			continue
		}
		filtered = append(filtered, line)
	}
	s, err := parseARCStats([]byte(strings.Join(filtered, "\n")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.L2Present {
		t.Fatal("expected l2_present=false when no l2_* counters reported")
	}
	if s.L2HitRatio != 0 {
		t.Fatalf("l2 hit ratio: got %.2f, want 0", s.L2HitRatio)
	}
}

func TestParseARCStats_EmptyRejected(t *testing.T) {
	// Just the two header lines — no counters.
	if _, err := parseARCStats([]byte("header1\nname type data\n")); err == nil {
		t.Fatal("expected error parsing empty kstat")
	}
}

func TestParseARCStats_ZeroTrafficHitRatio(t *testing.T) {
	// hits+misses == 0 must not panic or produce NaN.
	in := "header\nname type data\nhits 4 0\nmisses 4 0\nsize 4 1024\n"
	s, err := parseARCStats([]byte(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.HitRatio != 0 {
		t.Fatalf("hit ratio: got %.2f, want 0 on no traffic", s.HitRatio)
	}
}

func TestArcRecommendations_HealthyStaysQuiet(t *testing.T) {
	s := &ARCStats{Hits: 950_000, Misses: 50_000, HitRatio: 95, MaxSize: 1 << 30, TargetSize: 1 << 30}
	recs := arcRecommendations(s)
	if len(recs) != 0 {
		t.Fatalf("expected no recs for healthy ARC, got %v", recs)
	}
}

func TestArcRecommendations_LowHitRatioWarns(t *testing.T) {
	s := &ARCStats{Hits: 60_000, Misses: 40_000, HitRatio: 60, MaxSize: 1 << 30, TargetSize: 1 << 30}
	recs := arcRecommendations(s)
	if len(recs) == 0 {
		t.Fatal("expected at least one rec for 60% hit ratio")
	}
	joined := strings.Join(recs, "\n")
	if !strings.Contains(joined, "significantly undersized") {
		t.Fatalf("expected 'significantly undersized' message, got: %s", joined)
	}
}

func TestArcRecommendations_ModerateHitRatioMentions(t *testing.T) {
	s := &ARCStats{Hits: 800_000, Misses: 200_000, HitRatio: 80, MaxSize: 1 << 30, TargetSize: 1 << 30}
	recs := arcRecommendations(s)
	if len(recs) == 0 {
		t.Fatal("expected at least one rec for 80% hit ratio")
	}
	if !strings.Contains(strings.Join(recs, "\n"), "90%+") {
		t.Fatalf("expected 90%%+ reference, got: %v", recs)
	}
}

func TestArcRecommendations_LowTrafficSuppressed(t *testing.T) {
	// Below the 10k-op floor we stay quiet even with a bad ratio — not enough
	// traffic to draw a conclusion.
	s := &ARCStats{Hits: 100, Misses: 900, HitRatio: 10, MaxSize: 1 << 30, TargetSize: 1 << 30}
	if recs := arcRecommendations(s); len(recs) != 0 {
		t.Fatalf("expected no recs under traffic floor, got: %v", recs)
	}
}

func TestArcRecommendations_UnderutilizedL2ARC(t *testing.T) {
	s := &ARCStats{
		Hits: 1_000_000, Misses: 100_000, HitRatio: 90,
		MaxSize: 1 << 30, TargetSize: 1 << 30,
		L2Present: true, L2Hits: 1000, L2Misses: 200_000, L2HitRatio: 0.5,
	}
	recs := arcRecommendations(s)
	if len(recs) == 0 {
		t.Fatal("expected underutilized-L2ARC recommendation")
	}
	if !strings.Contains(strings.Join(recs, "\n"), "L2ARC") {
		t.Fatalf("expected L2ARC mention, got: %v", recs)
	}
}

func TestArcRecommendations_ARCMaxHeadroom(t *testing.T) {
	// Low hit ratio + target well under max → recommend raising zfs_arc_max.
	s := &ARCStats{
		Hits: 70_000, Misses: 30_000, HitRatio: 70,
		MaxSize: 16 << 30, TargetSize: 2 << 30,
	}
	recs := arcRecommendations(s)
	joined := strings.Join(recs, "\n")
	if !strings.Contains(joined, "zfs_arc_max") {
		t.Fatalf("expected zfs_arc_max recommendation, got: %v", recs)
	}
}
