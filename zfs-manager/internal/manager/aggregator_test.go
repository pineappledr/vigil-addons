package manager

import (
	"strings"
	"testing"
)

func TestDetectPoolTransitions_ResilverCompleted(t *testing.T) {
	prev := map[string]trackedPool{
		"tank": {ScrubStatus: "resilvering", NumDataVdevs: 2},
	}
	curr := map[string]trackedPool{
		"tank": {ScrubStatus: "completed", NumDataVdevs: 2},
	}
	events := detectPoolTransitions(prev, curr)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].EventType != "resilver_completed" {
		t.Errorf("event type = %q, want resilver_completed", events[0].EventType)
	}
	if events[0].PoolName != "tank" {
		t.Errorf("pool name = %q, want tank", events[0].PoolName)
	}
	if !strings.Contains(events[0].Message, "tank") {
		t.Errorf("message should mention pool name, got %q", events[0].Message)
	}
}

func TestDetectPoolTransitions_ResilverStillRunning(t *testing.T) {
	prev := map[string]trackedPool{"tank": {ScrubStatus: "resilvering", NumDataVdevs: 2}}
	curr := map[string]trackedPool{"tank": {ScrubStatus: "resilvering", NumDataVdevs: 2}}
	if events := detectPoolTransitions(prev, curr); len(events) != 0 {
		t.Errorf("expected no events while resilver still running, got %+v", events)
	}
}

func TestDetectPoolTransitions_PoolExpansion(t *testing.T) {
	prev := map[string]trackedPool{"tank": {ScrubStatus: "none", NumDataVdevs: 2}}
	curr := map[string]trackedPool{"tank": {ScrubStatus: "none", NumDataVdevs: 3}}
	events := detectPoolTransitions(prev, curr)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].EventType != "pool_expansion_completed" {
		t.Errorf("event type = %q, want pool_expansion_completed", events[0].EventType)
	}
	if !strings.Contains(events[0].Message, "tank") || !strings.Contains(events[0].Message, "2") || !strings.Contains(events[0].Message, "3") {
		t.Errorf("message should reference pool and counts, got %q", events[0].Message)
	}
}

func TestDetectPoolTransitions_PoolShrink(t *testing.T) {
	// vdev removal isn't a notify-worthy state for this gap — confirm we
	// don't fire a spurious "expansion" event when the count goes down.
	prev := map[string]trackedPool{"tank": {ScrubStatus: "none", NumDataVdevs: 3}}
	curr := map[string]trackedPool{"tank": {ScrubStatus: "none", NumDataVdevs: 2}}
	if events := detectPoolTransitions(prev, curr); len(events) != 0 {
		t.Errorf("expected no events on vdev removal, got %+v", events)
	}
}

func TestDetectPoolTransitions_NewPoolBaseline(t *testing.T) {
	// A pool that wasn't in the previous snapshot should not generate events
	// — we have no baseline so we don't know if a transition happened.
	prev := map[string]trackedPool{}
	curr := map[string]trackedPool{"tank": {ScrubStatus: "resilvering", NumDataVdevs: 2}}
	if events := detectPoolTransitions(prev, curr); len(events) != 0 {
		t.Errorf("expected no events for new pool, got %+v", events)
	}
}

func TestDetectPoolTransitions_MultiplePools(t *testing.T) {
	prev := map[string]trackedPool{
		"tank": {ScrubStatus: "resilvering", NumDataVdevs: 2},
		"data": {ScrubStatus: "none", NumDataVdevs: 4},
	}
	curr := map[string]trackedPool{
		"tank": {ScrubStatus: "completed", NumDataVdevs: 2},
		"data": {ScrubStatus: "none", NumDataVdevs: 5},
	}
	events := detectPoolTransitions(prev, curr)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.EventType+":"+e.PoolName] = true
	}
	if !seen["resilver_completed:tank"] {
		t.Errorf("missing resilver_completed for tank, got %+v", events)
	}
	if !seen["pool_expansion_completed:data"] {
		t.Errorf("missing pool_expansion_completed for data, got %+v", events)
	}
}

func TestCountDataVdevs_IgnoresSpecialVdevs(t *testing.T) {
	vdevs := []vdevMin{
		{Name: "mirror-0", Type: "mirror"},
		{Name: "mirror-1", Type: "mirror"},
		{Name: "cache", Type: "cache"},
		{Name: "logs", Type: "log"},
		{Name: "spares", Type: "spare"},
		{Name: "special-0", Type: "special"},
	}
	if got := countDataVdevs(vdevs); got != 2 {
		t.Errorf("countDataVdevs = %d, want 2", got)
	}
}

func TestCountDataVdevs_Empty(t *testing.T) {
	if got := countDataVdevs(nil); got != 0 {
		t.Errorf("countDataVdevs(nil) = %d, want 0", got)
	}
}

func TestCountDataVdevs_AllData(t *testing.T) {
	vdevs := []vdevMin{
		{Name: "raidz1-0", Type: "raidz1"},
		{Name: "raidz1-1", Type: "raidz1"},
		{Name: "raidz1-2", Type: "raidz1"},
	}
	if got := countDataVdevs(vdevs); got != 3 {
		t.Errorf("countDataVdevs = %d, want 3", got)
	}
}

func TestIsResilvering(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"resilvering", true},
		{"resilvering (38.93% done)", true},
		{"completed", false},
		{"in_progress", false},
		{"none", false},
		{"paused", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isResilvering(tt.in); got != tt.want {
			t.Errorf("isResilvering(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
