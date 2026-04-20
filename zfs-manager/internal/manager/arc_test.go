package manager

import (
	"strings"
	"testing"
)

func TestBuildARCMetricRows_IncludesCoreMetrics(t *testing.T) {
	p := &arcPayload{
		Size: 1 << 30, TargetSize: 1 << 30, MaxSize: 2 << 30, MinSize: 1 << 28,
		Hits: 9000, Misses: 1000, HitRatio: 90,
	}
	rows := buildARCMetricRows(p)

	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Category+"/"+r.Metric] = true
	}
	for _, want := range []string{
		"ARC/Hit Ratio",
		"ARC/Size",
		"ARC/Target Size",
		"ARC/Max Size",
		"ARC/Min Size",
		"ARC/Hits",
		"ARC/Misses",
	} {
		if !seen[want] {
			t.Errorf("missing metric row: %s", want)
		}
	}
	for _, unwanted := range []string{"L2ARC/Hit Ratio", "L2ARC/Size"} {
		if seen[unwanted] {
			t.Errorf("L2ARC row %q must not be emitted when l2_present is false", unwanted)
		}
	}
}

func TestBuildARCMetricRows_L2ARCRowsWhenPresent(t *testing.T) {
	p := &arcPayload{
		Size: 1 << 30, MaxSize: 2 << 30,
		Hits: 9000, Misses: 1000, HitRatio: 90,
		L2Present: true, L2Size: 10 << 30, L2Asize: 5 << 30,
		L2Hits: 800, L2Misses: 200, L2HitRatio: 80,
	}
	rows := buildARCMetricRows(p)
	var gotL2Ratio bool
	for _, r := range rows {
		if r.Category == "L2ARC" && r.Metric == "Hit Ratio" {
			gotL2Ratio = true
			if !strings.Contains(r.Value, "80.0") {
				t.Errorf("L2 hit ratio rendered as %q, expected ~80.0%%", r.Value)
			}
		}
	}
	if !gotL2Ratio {
		t.Error("expected L2ARC Hit Ratio row when l2_present is true")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1 << 20, "1.00 MiB"},
		{1 << 30, "1.00 GiB"},
		{1 << 40, "1.00 TiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{999, "999"},
		{1500, "1.50k"},
		{2_500_000, "2.50M"},
		{3_200_000_000, "3.20G"},
	}
	for _, tc := range cases {
		if got := formatCount(tc.n); got != tc.want {
			t.Errorf("formatCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
