package manager

import (
	"strings"
	"testing"
)

func TestBuildPropertyCommand_SortedDeterministic(t *testing.T) {
	changes := map[string]string{
		"recordsize":  "128K",
		"compression": "lz4",
		"atime":       "off",
	}
	cmd1 := buildPropertyCommand("dataset", "tank/work", changes)
	cmd2 := buildPropertyCommand("dataset", "tank/work", changes)
	if cmd1 != cmd2 {
		t.Fatalf("non-deterministic output:\n%s\n---\n%s", cmd1, cmd2)
	}
	// Sorted keys: atime, compression, recordsize
	want := "zfs set atime=off tank/work\nzfs set compression=lz4 tank/work\nzfs set recordsize=128K tank/work"
	if cmd1 != want {
		t.Fatalf("unexpected command output.\ngot:\n%s\nwant:\n%s", cmd1, want)
	}
}

func TestBuildPropertyCommand_PoolUsesZpool(t *testing.T) {
	cmd := buildPropertyCommand("pool", "tank", map[string]string{"autotrim": "on"})
	if !strings.HasPrefix(cmd, "zpool set ") {
		t.Fatalf("expected zpool prefix, got %q", cmd)
	}
}

func TestBuildPropertyCommand_EmptyChanges(t *testing.T) {
	if got := buildPropertyCommand("dataset", "tank", map[string]string{}); got != "" {
		t.Fatalf("expected empty string for no changes, got %q", got)
	}
}

func TestNormalizePropertyValue_TrimsAndLowercases(t *testing.T) {
	cases := map[string]string{
		"  LZ4  ":      "lz4",
		"On":           "on",
		"128K":         "128k",
		"":             "",
		"   ":          "",
		"mixed\tCASE ": "mixed\tcase",
	}
	for in, want := range cases {
		if got := normalizePropertyValue(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTierRank_Ordering(t *testing.T) {
	if tierRank("red") <= tierRank("yellow") {
		t.Error("red should outrank yellow")
	}
	if tierRank("yellow") <= tierRank("green") {
		t.Error("yellow should outrank green")
	}
	if tierRank("") != 0 || tierRank("mystery") != 0 {
		t.Error("unknown tiers should rank as 0")
	}
}

func TestKeysOf_ReturnsAllKeys(t *testing.T) {
	m := map[string]string{"b": "2", "a": "1", "c": "3"}
	keys := keysOf(m)
	if len(keys) != 3 {
		t.Fatalf("want 3 keys, got %d", len(keys))
	}
	sortStrings(keys)
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if keys[i] != w {
			t.Errorf("keys[%d] = %q, want %q", i, keys[i], w)
		}
	}
}

func TestSortStrings_AlreadySorted(t *testing.T) {
	s := []string{"a", "b", "c"}
	sortStrings(s)
	if s[0] != "a" || s[1] != "b" || s[2] != "c" {
		t.Errorf("sorted slice changed: %v", s)
	}
}

func TestSortStrings_ReverseSorted(t *testing.T) {
	s := []string{"c", "b", "a"}
	sortStrings(s)
	if s[0] != "a" || s[1] != "b" || s[2] != "c" {
		t.Errorf("unsorted output: %v", s)
	}
}

func TestHubPropertyCatalog_HasRedEntries(t *testing.T) {
	cat := hubPropertyCatalog()
	var sawDedupRed, sawFailmodeRed bool
	for _, e := range cat {
		if e.Key == "dedup" && e.Scope == "dataset" && e.Tier == "red" {
			sawDedupRed = true
		}
		if e.Key == "failmode" && e.Scope == "pool" && e.Tier == "red" {
			sawFailmodeRed = true
		}
	}
	if !sawDedupRed {
		t.Error("dedup should be catalogued as red-tier dataset")
	}
	if !sawFailmodeRed {
		t.Error("failmode should be catalogued as red-tier pool")
	}
}
