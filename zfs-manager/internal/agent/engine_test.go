package agent

import (
	"strings"
	"testing"
)

func TestBuildReplaceCommand(t *testing.T) {
	got := BuildReplaceCommand("zpool", "tank", "sda", "/dev/sdb")
	want := "zpool replace tank sda /dev/sdb"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildAddVdevCommand_Mirror(t *testing.T) {
	got := BuildAddVdevCommand("zpool", "tank", "mirror", []string{"/dev/sda", "/dev/sdb"})
	want := "zpool add tank mirror /dev/sda /dev/sdb"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildAddVdevCommand_Stripe(t *testing.T) {
	got := BuildAddVdevCommand("zpool", "tank", "stripe", []string{"/dev/sda"})
	want := "zpool add tank /dev/sda"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildAddVdevCommand_Raidz1(t *testing.T) {
	got := BuildAddVdevCommand("zpool", "tank", "raidz1", []string{"/dev/sda", "/dev/sdb", "/dev/sdc"})
	want := "zpool add tank raidz1 /dev/sda /dev/sdb /dev/sdc"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOfflineCommand(t *testing.T) {
	got := BuildOfflineCommand("zpool", "tank", "sda")
	want := "zpool offline tank sda"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOnlineCommand(t *testing.T) {
	got := BuildOnlineCommand("zpool", "tank", "sda")
	want := "zpool online tank sda"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildClearCommand_Pool(t *testing.T) {
	got := BuildClearCommand("zpool", "tank", "")
	want := "zpool clear tank"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildClearCommand_Device(t *testing.T) {
	got := BuildClearCommand("zpool", "tank", "sda")
	want := "zpool clear tank sda"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildScrubCommand(t *testing.T) {
	tests := []struct {
		action string
		want   string
	}{
		{"start", "zpool scrub tank"},
		{"pause", "zpool scrub -p tank"},
		{"cancel", "zpool scrub -s tank"},
	}
	for _, tt := range tests {
		got := BuildScrubCommand("zpool", "tank", tt.action)
		if got != tt.want {
			t.Errorf("BuildScrubCommand(%q) = %q, want %q", tt.action, got, tt.want)
		}
	}
}

func TestBuildSnapshotCommand(t *testing.T) {
	got := BuildSnapshotCommand("zfs", "tank/data", "snap1", false)
	want := "zfs snapshot tank/data@snap1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	got = BuildSnapshotCommand("zfs", "tank/data", "snap1", true)
	want = "zfs snapshot -r tank/data@snap1"
	if got != want {
		t.Errorf("recursive: got %q, want %q", got, want)
	}
}

func TestBuildRollbackCommand(t *testing.T) {
	tests := []struct {
		depth string
		want  string
	}{
		{"latest", "zfs rollback tank/data@snap1"},
		{"intermediate", "zfs rollback -r tank/data@snap1"},
		{"all", "zfs rollback -R tank/data@snap1"},
	}
	for _, tt := range tests {
		got := BuildRollbackCommand("zfs", "tank/data@snap1", tt.depth)
		if got != tt.want {
			t.Errorf("depth=%q: got %q, want %q", tt.depth, got, tt.want)
		}
	}
}

func TestBuildDestroyDatasetCommand(t *testing.T) {
	got := BuildDestroyDatasetCommand("zfs", "tank/data", false)
	want := "zfs destroy tank/data"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	got = BuildDestroyDatasetCommand("zfs", "tank/data", true)
	want = "zfs destroy -r tank/data"
	if got != want {
		t.Errorf("recursive: got %q, want %q", got, want)
	}
}

// --- Phase 4: Preview warning helpers ---

func TestCheckVdevTypeMatch_Matches(t *testing.T) {
	existing := []VdevInfo{
		{Name: "mirror-0", Type: "mirror"},
		{Name: "mirror-1", Type: "mirror"},
	}
	if w := CheckVdevTypeMatch(existing, "mirror"); len(w) != 0 {
		t.Errorf("expected no warnings when types match, got %v", w)
	}
}

func TestCheckVdevTypeMatch_Mismatch(t *testing.T) {
	existing := []VdevInfo{{Name: "mirror-0", Type: "mirror"}}
	w := CheckVdevTypeMatch(existing, "raidz1")
	if len(w) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], "mirror") || !strings.Contains(w[0], "raidz1") {
		t.Errorf("warning should mention existing and proposed types, got %q", w[0])
	}
}

func TestCheckVdevTypeMatch_StripeNormalized(t *testing.T) {
	// "stripe" from the UI maps to "disk" internally — a pool of single-disk
	// vdevs should match a "stripe" proposal and produce no warnings.
	existing := []VdevInfo{{Name: "sda", Type: "disk"}}
	if w := CheckVdevTypeMatch(existing, "stripe"); len(w) != 0 {
		t.Errorf("stripe proposal against disk-type pool: got %v, want no warnings", w)
	}
	if w := CheckVdevTypeMatch(existing, ""); len(w) != 0 {
		t.Errorf("empty proposal (stripe default) against disk-type pool: got %v, want no warnings", w)
	}
}

func TestCheckVdevTypeMatch_IgnoresSpecialVdevs(t *testing.T) {
	// Cache, log, spare, and special vdevs legitimately coexist with any
	// data vdev type and should not count toward the match check.
	existing := []VdevInfo{
		{Name: "mirror-0", Type: "mirror"},
		{Name: "cache", Type: "cache"},
		{Name: "logs", Type: "log"},
		{Name: "spares", Type: "spare"},
	}
	if w := CheckVdevTypeMatch(existing, "mirror"); len(w) != 0 {
		t.Errorf("mirror against mirror+cache+log+spare: got %v, want no warnings", w)
	}
}

func TestCheckVdevTypeMatch_EmptyPool(t *testing.T) {
	// A pool with no data vdevs (e.g. couldn't parse status) should not
	// generate a warning — we don't know what's there.
	if w := CheckVdevTypeMatch(nil, "mirror"); len(w) != 0 {
		t.Errorf("empty pool: got %v, want no warnings", w)
	}
}

func TestCheckReplaceSize_LargerOK(t *testing.T) {
	if w := CheckReplaceSize(1_000_000_000, 2_000_000_000, "sda", "sdb"); len(w) != 0 {
		t.Errorf("larger replacement should not warn, got %v", w)
	}
}

func TestCheckReplaceSize_EqualOK(t *testing.T) {
	if w := CheckReplaceSize(1_000_000_000, 1_000_000_000, "sda", "sdb"); len(w) != 0 {
		t.Errorf("equal replacement should not warn, got %v", w)
	}
}

func TestCheckReplaceSize_SmallerWarns(t *testing.T) {
	w := CheckReplaceSize(2*1024*1024*1024*1024, 1*1024*1024*1024*1024, "sda", "sdb")
	if len(w) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], "sda") || !strings.Contains(w[0], "sdb") {
		t.Errorf("warning should name both devices, got %q", w[0])
	}
	if !strings.Contains(w[0], "TiB") {
		t.Errorf("warning should render human sizes, got %q", w[0])
	}
}

func TestCheckReplaceSize_UnknownSizeSkipped(t *testing.T) {
	// Either side being zero means lsblk couldn't resolve it — skip
	// the check rather than emit a misleading warning.
	if w := CheckReplaceSize(0, 1_000_000_000, "sda", "sdb"); len(w) != 0 {
		t.Errorf("unknown old size: got %v, want no warnings", w)
	}
	if w := CheckReplaceSize(1_000_000_000, 0, "sda", "sdb"); len(w) != 0 {
		t.Errorf("unknown new size: got %v, want no warnings", w)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{512, "512 B"},
		{2048, "2.00 KiB"},
		{2 * 1024 * 1024, "2.00 MiB"},
		{1_500_000_000, "1.40 GiB"},
		{2 * 1024 * 1024 * 1024 * 1024, "2.00 TiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.in); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseScrubInfo(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantLast   string
		wantStatus string
	}{
		{
			name:       "completed",
			input:      "  scan: scrub repaired 0B in 01:23:45 with 0 errors on Sun Apr  6 02:00:01 2026",
			wantLast:   "2026-04-06T02:00:01Z",
			wantStatus: "completed",
		},
		{
			name:       "in_progress",
			input:      "  scan: scrub in progress since Mon Apr  7 02:00:01 2026\n\t1.23T scanned at 456M/s, 789G issued at 123M/s, 2.00T total\n\t0B repaired, 38.93% done",
			wantStatus: "in_progress",
		},
		{
			name:       "none",
			input:      "  scan: none requested",
			wantLast:   "none",
			wantStatus: "none",
		},
		{
			name:       "paused",
			input:      "  scan: scrub paused since Mon Apr  7 03:00:00 2026",
			wantStatus: "paused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			last, status := parseScrubInfo(tt.input)
			if tt.wantLast != "" && last != tt.wantLast {
				t.Errorf("lastScrub = %q, want %q", last, tt.wantLast)
			}
			if status != tt.wantStatus {
				t.Errorf("scrubStatus = %q, want %q", status, tt.wantStatus)
			}
		})
	}
}
