package agent

import "testing"

func TestParsePropertyRows_HappyPath(t *testing.T) {
	out := "compression\tlz4\tlocal\nrecordsize\t131072\tdefault\n"
	rows := parsePropertyRows(out)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].Key != "compression" || rows[0].Value != "lz4" || rows[0].Source != "local" {
		t.Errorf("row 0 parsed wrong: %+v", rows[0])
	}
	if rows[1].Key != "recordsize" || rows[1].Value != "131072" || rows[1].Source != "default" {
		t.Errorf("row 1 parsed wrong: %+v", rows[1])
	}
}

func TestParsePropertyRows_MissingSource(t *testing.T) {
	out := "compression\tlz4\n"
	rows := parsePropertyRows(out)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Source != "" {
		t.Errorf("expected empty source, got %q", rows[0].Source)
	}
}

func TestParsePropertyRows_DropsMalformed(t *testing.T) {
	out := "compression\tlz4\tlocal\n\njunkline\nrecordsize\t131072\tdefault\n"
	rows := parsePropertyRows(out)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (junk dropped), got %d: %+v", len(rows), rows)
	}
}

func TestParsePropertyRows_Empty(t *testing.T) {
	if rows := parsePropertyRows(""); len(rows) != 0 {
		t.Fatalf("want 0 rows from empty input, got %d", len(rows))
	}
}

func TestDatasetPropertyKeys_MatchesCatalog(t *testing.T) {
	keys := datasetPropertyKeys()
	cat := BuildPropertyCatalog()
	if len(keys) != len(cat.Dataset) {
		t.Fatalf("dataset keys mismatch: got %d, catalog has %d", len(keys), len(cat.Dataset))
	}
}

func TestPoolPropertyKeys_MatchesCatalog(t *testing.T) {
	keys := poolPropertyKeys()
	cat := BuildPropertyCatalog()
	if len(keys) != len(cat.Pool) {
		t.Fatalf("pool keys mismatch: got %d, catalog has %d", len(keys), len(cat.Pool))
	}
}
