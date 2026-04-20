package agent

import (
	"strings"
	"testing"
)

func TestValidateProperty_SelectHappyPath(t *testing.T) {
	c := BuildPropertyCatalog()
	if err := ValidateProperty(c, ScopeDataset, "compression", "lz4"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateProperty(c, ScopePool, "autotrim", "on"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateProperty_UnknownKeyRejected(t *testing.T) {
	c := BuildPropertyCatalog()
	err := ValidateProperty(c, ScopeDataset, "no_such_prop", "anything")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected 'unknown' in error, got %v", err)
	}
}

func TestValidateProperty_BadSelectRejected(t *testing.T) {
	c := BuildPropertyCatalog()
	err := ValidateProperty(c, ScopeDataset, "compression", "bogus-algo")
	if err == nil {
		t.Fatal("expected error for invalid select value")
	}
}

func TestValidateProperty_EmptyValueRejected(t *testing.T) {
	c := BuildPropertyCatalog()
	if err := ValidateProperty(c, ScopeDataset, "atime", ""); err == nil {
		t.Fatal("expected error for empty value")
	}
}

func TestValidateProperty_SizeAcceptsCommonForms(t *testing.T) {
	c := BuildPropertyCatalog()
	good := []string{"none", "100G", "1T", "512", "1.5T", "10k", "10M"}
	for _, v := range good {
		if err := ValidateProperty(c, ScopeDataset, "quota", v); err != nil {
			t.Errorf("quota=%q: unexpected error: %v", v, err)
		}
	}
}

func TestValidateProperty_SizeRejectsGarbage(t *testing.T) {
	c := BuildPropertyCatalog()
	bad := []string{"100GG", "not-a-size", "10 G", "-5G"}
	for _, v := range bad {
		if err := ValidateProperty(c, ScopeDataset, "quota", v); err == nil {
			t.Errorf("quota=%q: expected error", v)
		}
	}
}

func TestValidateProperty_TextRejectsControlChars(t *testing.T) {
	c := BuildPropertyCatalog()
	if err := ValidateProperty(c, ScopePool, "comment", "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateProperty(c, ScopePool, "comment", "hello\nworld"); err == nil {
		t.Fatal("expected error for newline in text")
	}
	if err := ValidateProperty(c, ScopePool, "comment", "zero\x00byte"); err == nil {
		t.Fatal("expected error for NUL byte in text")
	}
}

func TestClassifyChange_GreenOnly(t *testing.T) {
	c := BuildPropertyCatalog()
	tier := ClassifyChange(c, ScopeDataset, map[string]string{
		"atime":   "off",
		"snapdir": "visible",
	})
	if tier != TierGreen {
		t.Fatalf("expected green, got %v", tier)
	}
}

func TestClassifyChange_MixedYellowWins(t *testing.T) {
	c := BuildPropertyCatalog()
	tier := ClassifyChange(c, ScopeDataset, map[string]string{
		"atime":       "off",
		"compression": "zstd",
	})
	if tier != TierYellow {
		t.Fatalf("expected yellow, got %v", tier)
	}
}

func TestClassifyChange_DedupIsRed(t *testing.T) {
	c := BuildPropertyCatalog()
	tier := ClassifyChange(c, ScopeDataset, map[string]string{
		"dedup": "on",
	})
	if tier != TierRed {
		t.Fatalf("expected red, got %v", tier)
	}
}

func TestClassifyChange_PoolFailmodeIsRed(t *testing.T) {
	c := BuildPropertyCatalog()
	tier := ClassifyChange(c, ScopePool, map[string]string{
		"failmode": "panic",
	})
	if tier != TierRed {
		t.Fatalf("expected red, got %v", tier)
	}
}

func TestClassifyChange_UnknownKeyForcesRed(t *testing.T) {
	c := BuildPropertyCatalog()
	tier := ClassifyChange(c, ScopeDataset, map[string]string{
		"atime":          "off",
		"mystery_unsafe": "whatever",
	})
	if tier != TierRed {
		t.Fatalf("expected red for unknown key, got %v", tier)
	}
}

func TestBuildPropertyCatalog_NoDuplicateKeysPerScope(t *testing.T) {
	c := BuildPropertyCatalog()
	check := func(scope string, defs []PropertyDef) {
		seen := make(map[string]bool, len(defs))
		for _, d := range defs {
			if seen[d.Key] {
				t.Errorf("%s catalog has duplicate key %q", scope, d.Key)
			}
			seen[d.Key] = true
		}
	}
	check("dataset", c.Dataset)
	check("pool", c.Pool)
}

func TestBuildPropertyCatalog_SelectOptionsPresent(t *testing.T) {
	c := BuildPropertyCatalog()
	for _, d := range append(append([]PropertyDef{}, c.Dataset...), c.Pool...) {
		if d.Type == "select" && len(d.Options) == 0 {
			t.Errorf("%s/%s is select-typed but has no options", d.Scope, d.Key)
		}
	}
}
