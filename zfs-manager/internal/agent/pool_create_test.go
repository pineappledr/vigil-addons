package agent

import (
	"strings"
	"testing"
)

func TestValidatePoolSpec_HappyMirror(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"sda", "sdb"}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidatePoolSpec_HappyRaidz2WithAux(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "raidz2", Devices: []string{"a", "b", "c", "d", "e"}},
			{Role: VdevRoleLog, Type: "mirror", Devices: []string{"log1", "log2"}},
			{Role: VdevRoleCache, Type: "stripe", Devices: []string{"cache1"}},
			{Role: VdevRoleSpare, Type: "stripe", Devices: []string{"spare1"}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidatePoolSpec_RejectsEmptyName(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Vdevs: []PoolVdevSpec{{Role: VdevRoleData, Type: "mirror", Devices: []string{"a", "b"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("expected empty-name error, got %v", err)
	}
}

func TestValidatePoolSpec_RejectsBadName(t *testing.T) {
	cases := []string{
		"1tank",     // leading digit
		"tank!",     // punctuation
		"tank name", // whitespace
		"tank.foo",  // dot
	}
	for _, name := range cases {
		err := ValidatePoolSpec(CreatePoolSpec{
			Name: name,
			Vdevs: []PoolVdevSpec{
				{Role: VdevRoleData, Type: "mirror", Devices: []string{"a", "b"}},
			},
		})
		if err == nil {
			t.Errorf("expected error for name %q", name)
		}
	}
}

func TestValidatePoolSpec_RejectsReservedName(t *testing.T) {
	for _, name := range []string{"mirror", "raidz1", "log", "cache"} {
		err := ValidatePoolSpec(CreatePoolSpec{
			Name: name,
			Vdevs: []PoolVdevSpec{
				{Role: VdevRoleData, Type: "mirror", Devices: []string{"a", "b"}},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("expected reserved-name error for %q, got %v", name, err)
		}
	}
}

func TestValidatePoolSpec_RejectsNoVdevs(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{Name: "tank"})
	if err == nil || !strings.Contains(err.Error(), "at least one vdev") {
		t.Fatalf("expected no-vdev error, got %v", err)
	}
}

func TestValidatePoolSpec_RejectsNoDataVdev(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleCache, Type: "stripe", Devices: []string{"sda"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "at least one data vdev") {
		t.Fatalf("expected no-data-vdev error, got %v", err)
	}
}

func TestValidatePoolSpec_MirrorNeeds2(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"sda"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("expected mirror-size error, got %v", err)
	}
}

func TestValidatePoolSpec_Raidz2Needs4(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "raidz2", Devices: []string{"a", "b", "c"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "at least 4") {
		t.Fatalf("expected raidz2-size error, got %v", err)
	}
}

func TestValidatePoolSpec_CacheMustBeStripe(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"a", "b"}},
			{Role: VdevRoleCache, Type: "mirror", Devices: []string{"c1", "c2"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "stripe") {
		t.Fatalf("expected cache-type error, got %v", err)
	}
}

func TestValidatePoolSpec_RejectsDuplicateDevice(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"sda", "sdb"}},
			{Role: VdevRoleLog, Type: "mirror", Devices: []string{"sdb", "sdc"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "appears in both") {
		t.Fatalf("expected duplicate-device error, got %v", err)
	}
}

func TestValidatePoolSpec_RejectsWhitespaceInDevice(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"sda", "bad name"}},
		},
	})
	if err == nil {
		t.Fatal("expected whitespace-in-device error")
	}
}

func TestValidatePoolSpec_AshiftRange(t *testing.T) {
	ok := ValidatePoolSpec(CreatePoolSpec{
		Name:   "tank",
		Ashift: 12,
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"a", "b"}},
		},
	})
	if ok != nil {
		t.Errorf("ashift 12 should be accepted: %v", ok)
	}
	bad := ValidatePoolSpec(CreatePoolSpec{
		Name:   "tank",
		Ashift: 20,
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"a", "b"}},
		},
	})
	if bad == nil {
		t.Error("ashift 20 should be rejected")
	}
}

func TestValidatePoolSpec_MountpointMustBeAbsolute(t *testing.T) {
	err := ValidatePoolSpec(CreatePoolSpec{
		Name:       "tank",
		Mountpoint: "relative/path",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"a", "b"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute-mountpoint error, got %v", err)
	}
}

func TestBuildCreatePoolCommand_SimpleMirror(t *testing.T) {
	spec := CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"sda", "sdb"}},
		},
	}
	got := BuildCreatePoolCommand("zpool", spec)
	want := "zpool create tank mirror sda sdb"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildCreatePoolCommand_FullFeatured(t *testing.T) {
	spec := CreatePoolSpec{
		Name:       "tank",
		Ashift:     12,
		Mountpoint: "/mnt/tank",
		Force:      true,
		FsProps:    map[string]string{"compression": "lz4", "atime": "off"},
		PoolProps:  map[string]string{"autotrim": "on"},
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "raidz2", Devices: []string{"a", "b", "c", "d"}},
			{Role: VdevRoleLog, Type: "mirror", Devices: []string{"log1", "log2"}},
			{Role: VdevRoleCache, Type: "stripe", Devices: []string{"cache1"}},
			{Role: VdevRoleSpare, Type: "stripe", Devices: []string{"spare1"}},
		},
	}
	got := BuildCreatePoolCommand("zpool", spec)
	// Expected: sorted -o props (autotrim, ashift=12 comes first since it's
	// emitted before PoolProps), sorted -O props (atime, compression), then
	// mountpoint, pool name, data vdev, then log/cache/spare in order.
	want := "zpool create -f -o ashift=12 -o autotrim=on -O atime=off -O compression=lz4 -m /mnt/tank tank raidz2 a b c d log mirror log1 log2 cache cache1 spare spare1"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildCreatePoolCommand_Deterministic(t *testing.T) {
	spec := CreatePoolSpec{
		Name: "tank",
		FsProps: map[string]string{
			"compression": "zstd",
			"atime":       "off",
			"recordsize":  "128K",
		},
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "mirror", Devices: []string{"sda", "sdb"}},
		},
	}
	first := BuildCreatePoolCommand("zpool", spec)
	for i := 0; i < 10; i++ {
		if got := BuildCreatePoolCommand("zpool", spec); got != first {
			t.Fatalf("non-deterministic output on iteration %d:\n%s\n---\n%s", i, first, got)
		}
	}
}

func TestBuildCreatePoolCommand_StripeOmitsKeyword(t *testing.T) {
	spec := CreatePoolSpec{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "stripe", Devices: []string{"sda", "sdb"}},
		},
	}
	got := BuildCreatePoolCommand("zpool", spec)
	if strings.Contains(got, "stripe") {
		t.Errorf("stripe keyword should be omitted, got %q", got)
	}
}

func TestCreatePoolRequest_ToSpec_FlatWizard(t *testing.T) {
	req := createPoolRequest{
		Name:         "tank",
		DataType:     "mirror",
		DataDevices:  []string{"sda", "sdb"},
		LogDevices:   []string{"log1"},
		LogType:      "stripe",
		CacheDevices: []string{"cache1"},
		Compression:  "lz4",
		Atime:        "off",
		Autotrim:     "on",
		Ashift:       12,
	}
	spec := req.toSpec()
	if len(spec.Vdevs) != 3 {
		t.Fatalf("want 3 vdevs (data+log+cache), got %d: %+v", len(spec.Vdevs), spec.Vdevs)
	}
	if spec.FsProps["compression"] != "lz4" || spec.FsProps["atime"] != "off" {
		t.Errorf("convenience fs_props not folded in: %+v", spec.FsProps)
	}
	if spec.PoolProps["autotrim"] != "on" {
		t.Errorf("autotrim not folded in: %+v", spec.PoolProps)
	}
}

func TestCreatePoolRequest_ToSpec_StructuredVdevsWin(t *testing.T) {
	req := createPoolRequest{
		Name: "tank",
		Vdevs: []PoolVdevSpec{
			{Role: VdevRoleData, Type: "raidz2", Devices: []string{"a", "b", "c", "d"}},
		},
		DataType:    "mirror",
		DataDevices: []string{"ignored1", "ignored2"},
	}
	spec := req.toSpec()
	if len(spec.Vdevs) != 1 || spec.Vdevs[0].Type != "raidz2" {
		t.Fatalf("structured vdevs should override flat: %+v", spec.Vdevs)
	}
}

func TestCreatePoolRequest_ToSpec_ExplicitMapBeatsConvenienceAlias(t *testing.T) {
	req := createPoolRequest{
		Name:        "tank",
		DataType:    "mirror",
		DataDevices: []string{"a", "b"},
		Compression: "lz4",
		FsProps:     map[string]string{"compression": "zstd"},
	}
	spec := req.toSpec()
	if spec.FsProps["compression"] != "zstd" {
		t.Errorf("explicit map should win over alias, got %q", spec.FsProps["compression"])
	}
}

func TestTrimDeviceList_DropsBlanks(t *testing.T) {
	in := []string{"sda", "", "  ", "sdb", "\t"}
	out := trimDeviceList(in)
	if len(out) != 2 || out[0] != "sda" || out[1] != "sdb" {
		t.Errorf("trimDeviceList dropped wrong entries: %+v", out)
	}
}
