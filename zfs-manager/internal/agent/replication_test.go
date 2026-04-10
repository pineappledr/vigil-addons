package agent

import "testing"

func TestBuildSendCommand(t *testing.T) {
	tests := []struct {
		name     string
		snap     string
		baseSnap string
		want     string
	}{
		{"full send", "pool/data@snap1", "", "/sbin/zfs send pool/data@snap1"},
		{"incremental", "pool/data@snap2", "pool/data@snap1", "/sbin/zfs send -i pool/data@snap1 pool/data@snap2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildSendCommand("/sbin/zfs", tt.snap, tt.baseSnap)
			if got != tt.want {
				t.Errorf("BuildSendCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildRecvCommand(t *testing.T) {
	tests := []struct {
		name   string
		target string
		force  bool
		want   string
	}{
		{"no force", "backup/data", false, "/sbin/zfs receive backup/data"},
		{"with force", "backup/data", true, "/sbin/zfs receive -F backup/data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildRecvCommand("/sbin/zfs", tt.target, tt.force)
			if got != tt.want {
				t.Errorf("BuildRecvCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildReplicationPipelineCommand(t *testing.T) {
	got := BuildReplicationPipelineCommand("zfs", "pool/data@snap2", "pool/data@snap1", "backup/data")
	want := "zfs send -i pool/data@snap1 pool/data@snap2 | zfs receive -F backup/data"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFindCommonSnapshot(t *testing.T) {
	src := []SnapshotInfo{
		{SnapName: "auto-2024-01-01", FullName: "pool/data@auto-2024-01-01"},
		{SnapName: "auto-2024-01-02", FullName: "pool/data@auto-2024-01-02"},
		{SnapName: "auto-2024-01-03", FullName: "pool/data@auto-2024-01-03"},
	}

	tests := []struct {
		name string
		dst  []SnapshotInfo
		want string
	}{
		{
			"no common",
			[]SnapshotInfo{{SnapName: "other-snap"}},
			"",
		},
		{
			"common oldest",
			[]SnapshotInfo{{SnapName: "auto-2024-01-01"}},
			"pool/data@auto-2024-01-01",
		},
		{
			"common newest preferred",
			[]SnapshotInfo{
				{SnapName: "auto-2024-01-01"},
				{SnapName: "auto-2024-01-02"},
			},
			"pool/data@auto-2024-01-02",
		},
		{
			"empty dst",
			nil,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindCommonSnapshot(src, tt.dst)
			if got != tt.want {
				t.Errorf("FindCommonSnapshot() = %q, want %q", got, tt.want)
			}
		})
	}
}
