package scheduler

import "testing"

func TestTruncateForNotification(t *testing.T) {
	// Under the limit: returned unchanged.
	if got := truncateForNotification("short", 100); got != "short" {
		t.Fatalf("short string changed: %q", got)
	}

	// Over the limit: result fits within n and carries the marker.
	big := ""
	for i := 0; i < 1000; i++ {
		big += "line of status output here\n"
	}
	out := truncateForNotification(big, 8*1024)
	if len(out) > 8*1024 {
		t.Fatalf("truncated output %d bytes exceeds cap 8192", len(out))
	}
	if !contains(out, "truncated") {
		t.Fatalf("missing truncation marker: ...%q", tail(out, 60))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
