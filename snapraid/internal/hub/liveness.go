package hub

import (
	"context"
	"fmt"
	"time"
)

// agentLivenessInterval is how often the hub re-evaluates which agents are
// online vs offline for notification purposes.
const agentLivenessInterval = 1 * time.Minute

// MonitorAgentLiveness watches the agent registry and emits a notification
// upstream whenever an agent transitions online → offline (warning) or
// offline → online (info). It runs until ctx is cancelled.
//
// This is the hub's job because the Vigil server only sees the *hub* addon's
// heartbeat — it has no visibility into the individual SnapRAID agents behind
// it. Without this, a wedged agent (e.g. one that can't register because of a
// PSK mismatch) would silently fall off the dashboard with no alert.
func (a *Aggregator) MonitorAgentLiveness(ctx context.Context) {
	ticker := time.NewTicker(agentLivenessInterval)
	defer ticker.Stop()

	// Seed the baseline so we don't fire a flood of notifications on startup
	// for agents that were already offline before the hub restarted.
	a.seedAgentLiveness()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.checkAgentLiveness()
		}
	}
}

// seedAgentLiveness records the current online/offline state of every known
// agent without emitting notifications.
func (a *Aggregator) seedAgentLiveness() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range a.registry.ListViews() {
		a.agentOnline[v.ID] = v.Status == "online"
	}
}

// checkAgentLiveness diffs the registry's current view against the last-known
// state and emits notifications for any transitions.
func (a *Aggregator) checkAgentLiveness() {
	a.mu.RLock()
	tc := a.telemetry
	a.mu.RUnlock()
	if tc == nil {
		return
	}

	for _, v := range a.registry.ListViews() {
		online := v.Status == "online"

		a.mu.Lock()
		prev, known := a.agentOnline[v.ID]
		a.agentOnline[v.ID] = online
		a.mu.Unlock()

		if !known || prev == online {
			continue
		}

		name := v.ID
		if v.Hostname != "" {
			name = v.Hostname
		}

		if online {
			a.emitNotification(tc, v.ID, "snapraid_agent_online", "info",
				fmt.Sprintf("SnapRAID agent %q is back online", name))
		} else {
			lastSeen := "never"
			if !v.LastSeenAt.IsZero() {
				lastSeen = v.LastSeenAt.Format(time.RFC3339)
			}
			a.emitNotification(tc, v.ID, "snapraid_agent_offline", "warning",
				fmt.Sprintf("SnapRAID agent %q is offline (last seen %s)", name, lastSeen))
		}
	}
}
