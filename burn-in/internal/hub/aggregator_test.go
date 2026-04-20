package hub

import (
	"testing"
	"time"
)

func TestRingAppend_UnderLimit(t *testing.T) {
	var buf []int
	for i := 0; i < 5; i++ {
		buf = ringAppend(buf, i, 10)
	}
	if len(buf) != 5 {
		t.Fatalf("expected 5 elements, got %d", len(buf))
	}
}

func TestRingAppend_Eviction(t *testing.T) {
	var buf []int
	maxLen := 100
	for i := 0; i < maxLen+1; i++ {
		buf = ringAppend(buf, i, maxLen)
	}
	// Should have evicted oldest 10% (10 items), then have 91 items total.
	expected := maxLen + 1 - maxLen/10
	if len(buf) != expected {
		t.Fatalf("expected %d elements after eviction, got %d", expected, len(buf))
	}
	// First element should be 10 (the 11th original element, since 0-9 were evicted).
	if buf[0] != maxLen/10 {
		t.Errorf("expected first element %d, got %d", maxLen/10, buf[0])
	}
}

func TestAgentFrame_ResolveTimestamp(t *testing.T) {
	f := agentFrame{Timestamp: "2025-01-15T10:00:00Z"}
	if got := f.resolveTimestamp(); got != "2025-01-15T10:00:00Z" {
		t.Errorf("resolveTimestamp with value: got %q", got)
	}

	f2 := agentFrame{}
	ts := f2.resolveTimestamp()
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("resolveTimestamp empty: got %q, not valid RFC3339: %v", ts, err)
	}
}

func TestAgentFrame_ResolveSource(t *testing.T) {
	f := agentFrame{Source: "custom-source", AgentID: "agent-1"}
	if got := f.resolveSource(); got != "custom-source" {
		t.Errorf("resolveSource with source: got %q, want %q", got, "custom-source")
	}

	f2 := agentFrame{AgentID: "agent-1"}
	if got := f2.resolveSource(); got != "agent-1" {
		t.Errorf("resolveSource fallback: got %q, want %q", got, "agent-1")
	}
}

func TestAgentFrame_ResolveLevel(t *testing.T) {
	f := agentFrame{Level: "warning", Severity: "info"}
	if got := f.resolveLevel(); got != "warning" {
		t.Errorf("resolveLevel prefers Level: got %q", got)
	}

	f2 := agentFrame{Severity: "error"}
	if got := f2.resolveLevel(); got != "error" {
		t.Errorf("resolveLevel fallback: got %q", got)
	}
}
