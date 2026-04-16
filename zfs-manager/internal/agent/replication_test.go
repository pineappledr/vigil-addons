package agent

import (
	"strings"
	"testing"
)

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

func TestBuildSSHArgs(t *testing.T) {
	tgt := RemoteTarget{
		Host:        "backup.lan",
		Port:        2222,
		User:        "zfs-recv",
		KeyPath:     "/data/ssh/backup",
		KnownHosts:  "/data/ssh/known_hosts",
		DestDataset: "tank/backups",
	}
	got := BuildSSHArgs(tgt)

	// Args must include: -i <key>, -o UserKnownHostsFile=<kh>,
	// -o StrictHostKeyChecking=accept-new, -o BatchMode=yes, -p <port>, user@host.
	joined := strings.Join(got, " ")
	must := []string{
		"-i /data/ssh/backup",
		"-o UserKnownHostsFile=/data/ssh/known_hosts",
		"-o StrictHostKeyChecking=accept-new",
		"-o BatchMode=yes",
		"-p 2222",
		"zfs-recv@backup.lan",
	}
	for _, m := range must {
		if !strings.Contains(joined, m) {
			t.Errorf("BuildSSHArgs() missing %q in %q", m, joined)
		}
	}

	// user@host must be the last arg so the caller can append the remote
	// command after it.
	if got[len(got)-1] != "zfs-recv@backup.lan" {
		t.Errorf("last arg = %q, want %q", got[len(got)-1], "zfs-recv@backup.lan")
	}
}

func TestBuildSSHArgs_DefaultPort(t *testing.T) {
	tgt := RemoteTarget{Host: "h", User: "u", Port: 0, KeyPath: "/k", KnownHosts: "/kh"}
	got := BuildSSHArgs(tgt)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-p 22") {
		t.Errorf("expected default -p 22, got %q", joined)
	}
}

func TestBuildPvArgs(t *testing.T) {
	tests := []struct {
		name string
		kbps int
		want []string
	}{
		{"disabled", 0, nil},
		{"negative treated as disabled", -1, nil},
		{"500 kbps", 500, []string{"-q", "-L", "500k"}},
		{"1024 kbps", 1024, []string{"-q", "-L", "1024k"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildPvArgs(tt.kbps)
			if tt.want == nil {
				if got != nil {
					t.Errorf("BuildPvArgs(%d) = %v, want nil", tt.kbps, got)
				}
				return
			}
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("BuildPvArgs(%d) = %v, want %v", tt.kbps, got, tt.want)
			}
		})
	}
}

func TestBuildRemoteReplicationPipeline(t *testing.T) {
	tgt := RemoteTarget{
		Host:        "h.lan",
		Port:        22,
		User:        "u",
		KeyPath:     "/k",
		KnownHosts:  "/kh",
		DestDataset: "tank/b",
	}

	t.Run("no bandwidth no pv", func(t *testing.T) {
		got := BuildRemoteReplicationPipeline("zfs", "ssh", "pool/d@s2", "pool/d@s1", tgt)
		if !strings.HasPrefix(got, "zfs send -i pool/d@s1 pool/d@s2 | ssh ") {
			t.Errorf("prefix mismatch: %q", got)
		}
		if !strings.HasSuffix(got, "u@h.lan zfs receive -F tank/b") {
			t.Errorf("suffix mismatch: %q", got)
		}
		if strings.Contains(got, "pv") {
			t.Errorf("unexpected pv stage when bandwidth=0: %q", got)
		}
	})

	t.Run("bandwidth with pv", func(t *testing.T) {
		tgtB := tgt
		tgtB.BandwidthKbps = 1024
		tgtB.PvPath = "/usr/bin/pv"
		got := BuildRemoteReplicationPipeline("zfs", "ssh", "pool/d@s2", "", tgtB)
		if !strings.Contains(got, "/usr/bin/pv -q -L 1024k") {
			t.Errorf("missing pv stage: %q", got)
		}
		// Full send (no -i on the send itself). The ssh stage has "-i /k" for
		// the private key, so only check the first pipe segment.
		sendStage := strings.SplitN(got, " | ", 2)[0]
		if strings.Contains(sendStage, "-i ") {
			t.Errorf("unexpected -i in full send: %q", sendStage)
		}
	})

	t.Run("bandwidth without pv silently drops cap", func(t *testing.T) {
		tgtNoPv := tgt
		tgtNoPv.BandwidthKbps = 1024
		tgtNoPv.PvPath = ""
		got := BuildRemoteReplicationPipeline("zfs", "ssh", "pool/d@s2", "", tgtNoPv)
		if strings.Contains(got, "pv") {
			t.Errorf("pv appeared despite empty PvPath: %q", got)
		}
	})
}

func TestSSHKeyNameValid(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"", false},
		{"ok", true},
		{"Ok-Name_9", true},
		{"has space", false},
		{"has/slash", false},
		{"dots.bad", false},
		{"../traversal", false},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshKeyNameValid(tt.name); got != tt.want {
				t.Errorf("sshKeyNameValid(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
