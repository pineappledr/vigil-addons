package agent

import "testing"

func TestValidateCron(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"empty rejected", "", true},
		{"whitespace-only rejected", "   ", true},
		{"every minute", "* * * * *", false},
		{"every 15 min", "*/15 * * * *", false},
		{"daily at midnight", "0 0 * * *", false},
		{"sunday 2am", "0 2 * * 0", false},
		{"bi-weekly", "0 0 1,15 * *", false},
		{"range", "0 9-17 * * 1-5", false},
		{"descriptor @daily rejected", "@daily", true},
		{"descriptor @every rejected", "@every 5m", true},
		{"wrong arity (4 fields)", "0 0 * *", true},
		{"junk value", "abc def", true},
		{"out of range minute", "99 * * * *", true},
		{"out of range hour", "0 25 * * *", true},
		{"out of range dom", "0 0 32 * *", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCron(tc.expr)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.expr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.expr, err)
			}
		})
	}
}
