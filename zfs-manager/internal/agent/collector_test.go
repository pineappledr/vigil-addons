package agent

import (
	"testing"
)

func TestCollectorEmitEvent_StoresAndFlushes(t *testing.T) {
	c := NewCollector("agent-1", "host", &Engine{}, discardLogger())

	evt := AgentEvent{
		ID:        "replace-tank-123",
		Type:      "drive_replacement_started",
		Severity:  "warning",
		Message:   "Drive replacement started: sda → sdb on pool tank",
		Timestamp: "2026-04-09T12:00:00Z",
	}
	c.EmitEvent(evt)

	// Cached event should appear on the next built payload.
	c.mu.RLock()
	got := c.lastEvent
	c.mu.RUnlock()
	if got == nil {
		t.Fatalf("lastEvent = nil, want stored event")
	}
	if got.ID != evt.ID || got.Type != evt.Type || got.Message != evt.Message {
		t.Errorf("lastEvent = %+v, want %+v", *got, evt)
	}

	// Flush channel should be signalled exactly once.
	select {
	case <-c.FlushCh():
	default:
		t.Errorf("flush channel was not signalled by EmitEvent")
	}
}

func TestCollectorEmitEvent_FlushChannelNonBlocking(t *testing.T) {
	// EmitEvent must not block when the flush channel is already full —
	// the next ticker iteration will pick the event up regardless.
	c := NewCollector("agent-1", "host", &Engine{}, discardLogger())

	c.RequestFlush() // fills the buffered chan
	c.EmitEvent(AgentEvent{ID: "evt-1", Type: "drive_replacement_started"})

	// Drain to confirm there's still exactly one signal pending.
	select {
	case <-c.FlushCh():
	default:
		t.Errorf("expected one buffered flush signal")
	}
}
