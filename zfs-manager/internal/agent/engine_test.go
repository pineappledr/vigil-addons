package agent

import "testing"

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
