package vigilclient

import "encoding/json"

// TelemetryFrame is the envelope for all frames sent upstream to Vigil.
type TelemetryFrame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NotificationPayload is the payload for a notification telemetry frame.
type NotificationPayload struct {
	EventType string `json:"event_type"`
	Severity  string `json:"severity"`
	Source    string `json:"source"`
	Host      string `json:"host"`
	Message   string `json:"message"`
	Timestamp string `json:"time"`
}

// LogPayload is the payload for a log telemetry frame.
// Field names match the Vigil UI contract: "level" and "source".
type LogPayload struct {
	ComponentID string `json:"component_id,omitempty"`
	Level       string `json:"level"`
	Message     string `json:"message"`
	Source      string `json:"source,omitempty"`
	JobID       string `json:"job_id,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}

// RegistrationResponse is the response from a successful addon registration.
type RegistrationResponse struct {
	AddonID   int64  `json:"addon_id"`
	SessionID string `json:"session_id"`
}
