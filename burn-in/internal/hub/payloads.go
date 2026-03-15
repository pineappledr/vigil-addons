package hub

import "encoding/json"

// ProgressPayload is the payload for a progress telemetry frame.
type ProgressPayload struct {
	AgentID      string          `json:"agent_id"`
	JobID        string          `json:"job_id"`
	Command      string          `json:"command"`
	Phase        string          `json:"phase"`
	PhaseDetail  string          `json:"phase_detail,omitempty"`
	Percent      float64         `json:"percent"`
	SpeedMbps    float64         `json:"speed_mbps,omitempty"`
	TempC        int             `json:"temp_c,omitempty"`
	ElapsedSec   int64           `json:"elapsed_sec"`
	ETASec       int64           `json:"eta_sec,omitempty"`
	BadblockErrs int             `json:"badblocks_errors,omitempty"`
	SmartDeltas  json.RawMessage `json:"smart_deltas,omitempty"`
}

// MetricPayload is the payload for a chart metric telemetry frame.
type MetricPayload struct {
	Key       string  `json:"key"`
	Value     float64 `json:"value"`
	Timestamp string  `json:"timestamp"`
}

// ChartPayload is the payload for a targeted chart telemetry frame.
// ComponentID identifies which chart component should receive the data point.
type ChartPayload struct {
	ComponentID string  `json:"component_id"`
	Key         string  `json:"key"`
	Value       float64 `json:"value"`
	Timestamp   string  `json:"timestamp"`
}
