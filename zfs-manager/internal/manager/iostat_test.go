package manager

import (
	"math"
	"testing"
)

func findRow(rows []iostatRow, scope, name string) *iostatRow {
	for i := range rows {
		if rows[i].Scope == scope && rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

func approx(a, b float64) bool {
	return math.Abs(a-b) < 0.001
}

func TestBuildIOStatRows_EmptyPrevYieldsZeroRates(t *testing.T) {
	curr := []iostatPool{
		{
			Name:       "tank",
			ReadOps:    100,
			WriteOps:   200,
			ReadBytes:  1 << 20,
			WriteBytes: 2 << 20,
			Timestamp:  "2026-04-17T00:00:10Z",
		},
	}
	rows := buildIOStatRows(curr, nil)
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Scope != "pool" || r.Name != "tank" {
		t.Fatalf("pool row: got %+v", r)
	}
	if r.ReadOps != 0 || r.WriteOps != 0 || r.ReadBytes != 0 || r.WriteBytes != 0 {
		t.Fatalf("rates must be zero without a prior sample, got %+v", r)
	}
	if r.TotalReadOps != 100 || r.TotalWriteBytes != 2<<20 {
		t.Fatalf("totals must pass through, got %+v", r)
	}
}

func TestBuildIOStatRows_ComputesRates(t *testing.T) {
	prev := []iostatPool{
		{
			Name:       "tank",
			ReadOps:    1000,
			WriteOps:   2000,
			ReadBytes:  10 << 20,
			WriteBytes: 20 << 20,
			Timestamp:  "2026-04-17T00:00:00Z",
			Vdevs: []iostatVdev{
				{
					Name: "mirror-0", Type: "mirror",
					ReadOps: 500, WriteOps: 1000,
					ReadBytes: 5 << 20, WriteBytes: 10 << 20,
					Disks: []iostatDisk{
						{Name: "sda", ReadOps: 250, WriteOps: 500, ReadBytes: 2 << 20, WriteBytes: 4 << 20},
					},
				},
			},
		},
	}
	curr := []iostatPool{
		{
			Name:       "tank",
			ReadOps:    1100,                   // +100 ops in 10s → 10 ops/s
			WriteOps:   2200,                   // +200 ops in 10s → 20 ops/s
			ReadBytes:  (10 << 20) + (5 << 20), // +5 MiB in 10s
			WriteBytes: (20 << 20) + (10 << 20),
			Timestamp:  "2026-04-17T00:00:10Z",
			Vdevs: []iostatVdev{
				{
					Name: "mirror-0", Type: "mirror",
					ReadOps: 550, WriteOps: 1100,
					ReadBytes: (5 << 20) + (2 << 20), WriteBytes: (10 << 20) + (5 << 20),
					Disks: []iostatDisk{
						{Name: "sda", ReadOps: 275, WriteOps: 550, ReadBytes: (2 << 20) + (1 << 20), WriteBytes: (4 << 20) + (2 << 20)},
					},
				},
			},
		},
	}
	rows := buildIOStatRows(curr, prev)

	pool := findRow(rows, "pool", "tank")
	if pool == nil {
		t.Fatal("missing pool row")
	}
	if !approx(pool.ReadOps, 10) {
		t.Errorf("pool read ops/s: got %v, want ~10", pool.ReadOps)
	}
	if !approx(pool.WriteOps, 20) {
		t.Errorf("pool write ops/s: got %v, want ~20", pool.WriteOps)
	}

	vdev := findRow(rows, "vdev", "mirror-0")
	if vdev == nil {
		t.Fatal("missing vdev row")
	}
	if vdev.Parent != "tank" || vdev.VdevType != "mirror" {
		t.Errorf("vdev metadata wrong: %+v", vdev)
	}
	if !approx(vdev.ReadOps, 5) {
		t.Errorf("vdev read ops/s: got %v, want ~5", vdev.ReadOps)
	}

	disk := findRow(rows, "disk", "sda")
	if disk == nil {
		t.Fatal("missing disk row")
	}
	if disk.Parent != "mirror-0" {
		t.Errorf("disk parent wrong: %+v", disk)
	}
	if !approx(disk.ReadOps, 2.5) {
		t.Errorf("disk read ops/s: got %v, want ~2.5", disk.ReadOps)
	}
}

func TestBuildIOStatRows_CounterResetYieldsZero(t *testing.T) {
	// Pool was exported/re-imported, counters dropped → rate must clamp to
	// zero, not underflow into a huge number.
	prev := []iostatPool{{Name: "tank", ReadOps: 1_000_000, Timestamp: "2026-04-17T00:00:00Z"}}
	curr := []iostatPool{{Name: "tank", ReadOps: 50, Timestamp: "2026-04-17T00:00:10Z"}}
	rows := buildIOStatRows(curr, prev)
	if rows[0].ReadOps != 0 {
		t.Fatalf("expected rate=0 on counter reset, got %v", rows[0].ReadOps)
	}
	if rows[0].TotalReadOps != 50 {
		t.Fatalf("totals should still reflect current sample, got %d", rows[0].TotalReadOps)
	}
}

func TestBuildIOStatRows_UnparsableTimestampsFallToZero(t *testing.T) {
	prev := []iostatPool{{Name: "tank", ReadOps: 100, Timestamp: "nope"}}
	curr := []iostatPool{{Name: "tank", ReadOps: 200, Timestamp: "also-nope"}}
	rows := buildIOStatRows(curr, prev)
	if rows[0].ReadOps != 0 {
		t.Fatalf("expected zero rate on unparsable timestamps, got %v", rows[0].ReadOps)
	}
}

func TestBuildIOStatRows_NewPoolWithoutPrevSample(t *testing.T) {
	// Pool exists in curr but not in prev (fresh pool creation) — rates must
	// be zero for it while other pools with history keep their rates.
	prev := []iostatPool{{Name: "tank", ReadOps: 100, Timestamp: "2026-04-17T00:00:00Z"}}
	curr := []iostatPool{
		{Name: "tank", ReadOps: 200, Timestamp: "2026-04-17T00:00:10Z"},
		{Name: "fresh", ReadOps: 500, Timestamp: "2026-04-17T00:00:10Z"},
	}
	rows := buildIOStatRows(curr, prev)
	tank := findRow(rows, "pool", "tank")
	fresh := findRow(rows, "pool", "fresh")
	if tank == nil || fresh == nil {
		t.Fatal("missing rows")
	}
	if !approx(tank.ReadOps, 10) {
		t.Errorf("tank rate: got %v, want 10", tank.ReadOps)
	}
	if fresh.ReadOps != 0 {
		t.Errorf("fresh pool rate must be 0, got %v", fresh.ReadOps)
	}
	if fresh.TotalReadOps != 500 {
		t.Errorf("fresh pool totals pass through, got %d", fresh.TotalReadOps)
	}
}

func TestDeltaSeconds(t *testing.T) {
	cases := []struct {
		name     string
		curr     string
		prev     string
		have     bool
		wantZero bool
		want     float64
	}{
		{"no prev", "2026-04-17T00:00:10Z", "", false, true, 0},
		{"valid 10s", "2026-04-17T00:00:10Z", "2026-04-17T00:00:00Z", true, false, 10},
		{"zero delta", "2026-04-17T00:00:00Z", "2026-04-17T00:00:00Z", true, true, 0},
		{"negative delta", "2026-04-17T00:00:00Z", "2026-04-17T00:00:10Z", true, true, 0},
		{"bad curr", "nope", "2026-04-17T00:00:00Z", true, true, 0},
		{"bad prev", "2026-04-17T00:00:00Z", "nope", true, true, 0},
	}
	for _, tc := range cases {
		got := deltaSeconds(tc.curr, tc.prev, tc.have)
		if tc.wantZero {
			if got != 0 {
				t.Errorf("%s: got %v, want 0", tc.name, got)
			}
		} else if !approx(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
