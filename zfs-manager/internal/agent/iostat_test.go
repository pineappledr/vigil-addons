package agent

import (
	"strings"
	"testing"
)

// Synthetic `zpool iostat -Hpv` output for a single-mirror pool. Fields are
// separated by tabs, matching the -H contract.
const sampleIOStatMirror = "tank\t" + "1048576\t2097152\t100\t200\t1048576\t2097152\n" +
	"mirror-0\t" + "1048576\t2097152\t100\t200\t1048576\t2097152\n" +
	"/dev/sda\t" + "-\t-\t50\t100\t524288\t1048576\n" +
	"/dev/sdb\t" + "-\t-\t50\t100\t524288\t1048576\n"

func TestParseIOStat_MirrorWithLeafDisks(t *testing.T) {
	pools, err := parseIOStat(sampleIOStatMirror)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("pools: got %d, want 1", len(pools))
	}
	p := pools[0]
	if p.Name != "tank" || p.ReadOps != 100 || p.WriteOps != 200 {
		t.Fatalf("pool counters wrong: %+v", p)
	}
	if len(p.Vdevs) != 1 {
		t.Fatalf("vdevs: got %d, want 1", len(p.Vdevs))
	}
	v := p.Vdevs[0]
	if v.Name != "mirror-0" || v.Type != "mirror" {
		t.Fatalf("vdev metadata wrong: %+v", v)
	}
	if len(v.Disks) != 2 {
		t.Fatalf("disks: got %d, want 2", len(v.Disks))
	}
	if v.Disks[0].Name != "/dev/sda" || v.Disks[0].ReadOps != 50 {
		t.Fatalf("disk[0] wrong: %+v", v.Disks[0])
	}
}

// Two-pool output where the second pool is a single-disk stripe (the disk row
// IS the vdev because there's no mirror/raidz row between pool and leaf).
const sampleIOStatTwoPools = "tank\t" + "100\t200\t10\t20\t1000\t2000\n" +
	"mirror-0\t" + "100\t200\t10\t20\t1000\t2000\n" +
	"/dev/sda\t" + "-\t-\t5\t10\t500\t1000\n" +
	"/dev/sdb\t" + "-\t-\t5\t10\t500\t1000\n" +
	"scratch\t" + "50\t100\t5\t5\t250\t500\n" +
	"/dev/sdc\t" + "-\t-\t5\t5\t250\t500\n"

func TestParseIOStat_TwoPoolsSecondStripe(t *testing.T) {
	pools, err := parseIOStat(sampleIOStatTwoPools)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("pools: got %d, want 2", len(pools))
	}
	if pools[0].Name != "tank" || pools[1].Name != "scratch" {
		t.Fatalf("pool order: got [%s, %s]", pools[0].Name, pools[1].Name)
	}
	// scratch has one single-disk stripe vdev synthesized from the leaf row.
	if len(pools[1].Vdevs) != 1 {
		t.Fatalf("scratch vdevs: got %d, want 1", len(pools[1].Vdevs))
	}
	v := pools[1].Vdevs[0]
	if v.Name != "/dev/sdc" || v.Type != "disk" {
		t.Fatalf("scratch stripe vdev wrong: %+v", v)
	}
}

const sampleIOStatRaidz2 = "pool1\t" + "1000\t2000\t100\t200\t1000000\t2000000\n" +
	"raidz2-0\t" + "1000\t2000\t100\t200\t1000000\t2000000\n" +
	"/dev/sda\t" + "-\t-\t25\t50\t250000\t500000\n" +
	"/dev/sdb\t" + "-\t-\t25\t50\t250000\t500000\n" +
	"/dev/sdc\t" + "-\t-\t25\t50\t250000\t500000\n" +
	"/dev/sdd\t" + "-\t-\t25\t50\t250000\t500000\n"

func TestParseIOStat_Raidz2TypeClassified(t *testing.T) {
	pools, err := parseIOStat(sampleIOStatRaidz2)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v := pools[0].Vdevs[0]
	if v.Type != "raidz2" {
		t.Fatalf("raidz2 vdev type: got %q, want %q", v.Type, "raidz2")
	}
	if len(v.Disks) != 4 {
		t.Fatalf("raidz2 disks: got %d, want 4", len(v.Disks))
	}
}

func TestParseIOStat_EmptyRejected(t *testing.T) {
	if _, err := parseIOStat(""); err == nil {
		t.Fatal("expected error on empty input")
	}
	if _, err := parseIOStat("\n\n"); err == nil {
		t.Fatal("expected error on blank-only input")
	}
}

func TestParseIOStat_SampleIOStatTimestampPassthrough(t *testing.T) {
	// parseIOStat alone doesn't assign timestamps — SampleIOStat does. Just
	// assert the parser leaves Timestamp empty so SampleIOStat's assignment
	// doesn't race with a pre-existing value.
	pools, err := parseIOStat(sampleIOStatMirror)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, p := range pools {
		if p.Timestamp != "" {
			t.Fatalf("parser should not set Timestamp; got %q", p.Timestamp)
		}
	}
}

func TestClassifyVdevType(t *testing.T) {
	cases := map[string]string{
		"mirror-0":   "mirror",
		"raidz1-0":   "raidz1",
		"raidz2-3":   "raidz2",
		"raidz3-0":   "raidz3",
		"raidz":      "raidz",
		"cache":      "cache",
		"log":        "log",
		"spare":      "spare",
		"special":    "special",
		"dedup":      "dedup",
		"/dev/sda":   "disk",
		"wwn-abc123": "disk",
	}
	for name, want := range cases {
		if got := classifyVdevType(name); got != want {
			t.Errorf("classifyVdevType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestLooksLikeDisk(t *testing.T) {
	// Keep disk-leaf heuristic honest — any common leaf spelling should match.
	wantTrue := []string{
		"/dev/sda", "/dev/nvme0n1", "sda", "sdb1", "nvme0n1p1", "vda",
		"xvdf", "hda", "wwn-0x500003960b123456", "scsi-35000...", "ata-ST4000...",
	}
	for _, n := range wantTrue {
		if !looksLikeDisk(n) {
			t.Errorf("looksLikeDisk(%q) = false, want true", n)
		}
	}
	wantFalse := []string{"tank", "mirror-0", "raidz2-0", "cache", "log", "spare"}
	for _, n := range wantFalse {
		if looksLikeDisk(n) {
			t.Errorf("looksLikeDisk(%q) = true, want false", n)
		}
	}
}

func TestParseIOStat_TolerantOfBlankLines(t *testing.T) {
	// Blank lines between pools must not end parsing — some zfs builds emit
	// them and the parser contract is to keep going.
	input := sampleIOStatMirror + "\n\n" + "scratch\t50\t100\t5\t5\t250\t500\n" +
		"/dev/sdc\t-\t-\t5\t5\t250\t500\n"
	pools, err := parseIOStat(input)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("pools: got %d (%s), want 2", len(pools), poolNames(pools))
	}
}

func poolNames(pools []PoolIOStat) string {
	names := make([]string, len(pools))
	for i, p := range pools {
		names[i] = p.Name
	}
	return strings.Join(names, ",")
}
