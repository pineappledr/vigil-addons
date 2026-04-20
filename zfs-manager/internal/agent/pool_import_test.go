package agent

import (
	"strings"
	"testing"
)

func TestParseImportablePools_SinglePool(t *testing.T) {
	out := `
   pool: tank
     id: 12345678901234567890
  state: ONLINE
 status: The pool is healthy.
 action: The pool can be imported using its name or numeric identifier.
 config:

        tank         ONLINE
          mirror-0   ONLINE
            sda1     ONLINE
            sdb1     ONLINE
`
	pools := parseImportablePools(out)
	if len(pools) != 1 {
		t.Fatalf("want 1 pool, got %d", len(pools))
	}
	got := pools[0]
	if got.Name != "tank" {
		t.Errorf("name = %q, want tank", got.Name)
	}
	if got.ID != "12345678901234567890" {
		t.Errorf("id = %q", got.ID)
	}
	if got.State != "ONLINE" {
		t.Errorf("state = %q", got.State)
	}
	if !strings.Contains(got.Status, "healthy") {
		t.Errorf("status = %q", got.Status)
	}
	if !strings.Contains(got.Action, "imported") {
		t.Errorf("action = %q", got.Action)
	}
}

func TestParseImportablePools_TwoPools(t *testing.T) {
	out := `
   pool: tank
     id: 111
  state: ONLINE
 config:
        tank ONLINE

   pool: backup
     id: 222
  state: DEGRADED
 status: One or more devices could not be used.
 action: Attach the missing device.
 config:
        backup DEGRADED
`
	pools := parseImportablePools(out)
	if len(pools) != 2 {
		t.Fatalf("want 2 pools, got %d: %+v", len(pools), pools)
	}
	if pools[0].Name != "tank" || pools[0].ID != "111" || pools[0].State != "ONLINE" {
		t.Errorf("pool 0 wrong: %+v", pools[0])
	}
	if pools[1].Name != "backup" || pools[1].ID != "222" || pools[1].State != "DEGRADED" {
		t.Errorf("pool 1 wrong: %+v", pools[1])
	}
	if !strings.Contains(pools[1].Status, "could not be used") {
		t.Errorf("pool 1 status = %q", pools[1].Status)
	}
}

func TestParseImportablePools_EmptyInput(t *testing.T) {
	pools := parseImportablePools("")
	if len(pools) != 0 {
		t.Fatalf("want 0 pools for empty input, got %d", len(pools))
	}
}

func TestParseImportablePools_IgnoresConfigBody(t *testing.T) {
	// The device config block uses its own whitespace layout and should not be
	// mistaken for key:value lines. The only key-looking rows it contains are
	// "NAME  STATE  READ WRITE CKSUM" style headers that have no colons.
	out := `
   pool: tank
     id: 1
  state: ONLINE
 config:

	NAME       STATE
	tank       ONLINE
	  sda1     ONLINE
`
	pools := parseImportablePools(out)
	if len(pools) != 1 {
		t.Fatalf("want 1 pool, got %d", len(pools))
	}
	if pools[0].Name != "tank" {
		t.Errorf("parser picked up something other than the pool header: %+v", pools[0])
	}
}

func TestParseImportablePools_CommentAndBlankLines(t *testing.T) {
	out := `
   pool: archive
     id: 42
  state: ONLINE
comment: cold storage node
`
	pools := parseImportablePools(out)
	if len(pools) != 1 {
		t.Fatalf("want 1 pool, got %d", len(pools))
	}
	if pools[0].Comment != "cold storage node" {
		t.Errorf("comment = %q", pools[0].Comment)
	}
}

func TestSplitImportKV_HappyPath(t *testing.T) {
	k, v, ok := splitImportKV("pool: tank")
	if !ok || k != "pool" || v != "tank" {
		t.Errorf("unexpected: k=%q v=%q ok=%v", k, v, ok)
	}
}

func TestSplitImportKV_RejectsUnknownKey(t *testing.T) {
	_, _, ok := splitImportKV("NAME: STATE")
	if ok {
		t.Error("expected 'NAME: STATE' to be rejected (not in whitelist)")
	}
}

func TestSplitImportKV_RejectsNoColon(t *testing.T) {
	_, _, ok := splitImportKV("just a line")
	if ok {
		t.Error("expected lines with no colon to be rejected")
	}
}

func TestSplitImportKV_RejectsEmptyValue(t *testing.T) {
	_, _, ok := splitImportKV("config:")
	if ok {
		t.Error("expected 'config:' (no value) to be rejected")
	}
}
