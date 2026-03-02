package agent

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
)

const maxPayloadSize = 1 << 20 // 1 MB

// ExecutePayload is the signed command payload received from the hub.
type ExecutePayload struct {
	AgentID   string          `json:"agent_id"`
	Command   string          `json:"command"`
	Target    string          `json:"target"`
	Params    json.RawMessage `json:"params,omitempty"`
	Signature string          `json:"signature"`
}

// JobCancelFunc is the function to call to cancel an active job.
type JobCancelFunc = func()

// JobDispatcher starts a job and returns its ID.
// Implemented by jobs.JobManager.
type JobDispatcher interface {
	StartJob(cmd JobCommand) (string, error)
}

// JobCommand is the inbound command for job dispatch.
// Mirrors the structure expected by the job manager.
type JobCommand struct {
	AgentID string          `json:"agent_id"`
	Command string          `json:"command"`
	Target  string          `json:"target"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// AgentAPI is the HTTP server for the burn-in agent.
type AgentAPI struct {
	serverPubkey ed25519.PublicKey
	logger       *slog.Logger
	dispatcher   JobDispatcher

	mu         sync.Mutex
	activeJobs map[string]JobCancelFunc
}

// SetJobDispatcher injects the job manager after construction to break
// the circular dependency (JobManager needs AgentAPI for lifecycle callbacks,
// AgentAPI needs JobManager for dispatch).
func (a *AgentAPI) SetJobDispatcher(d JobDispatcher) {
	a.dispatcher = d
}

// NewAgentAPI creates the agent API server.
// pubkeyPath is the path to the Vigil server's Ed25519 public key file.
// If empty, signature verification is disabled (for development only).
func NewAgentAPI(pubkeyPath string, logger *slog.Logger) (*AgentAPI, error) {
	api := &AgentAPI{
		logger:     logger,
		activeJobs: make(map[string]JobCancelFunc),
	}

	if pubkeyPath != "" {
		key, err := loadEd25519PublicKey(pubkeyPath)
		if err != nil {
			return nil, fmt.Errorf("loading server public key: %w", err)
		}
		api.serverPubkey = key
		logger.Info("ed25519 signature verification enabled", "pubkey", pubkeyPath)
	} else {
		logger.Warn("no server_pubkey configured, signature verification disabled")
	}

	return api, nil
}

// Handler returns an http.ServeMux with all agent routes registered.
func (a *AgentAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("POST /api/execute", a.handleExecute)
	mux.HandleFunc("DELETE /api/jobs/{id}", a.handleAbortJob)
	return mux
}

// RegisterJob tracks an active job's cancel function so it can be aborted.
func (a *AgentAPI) RegisterJob(jobID string, cancel JobCancelFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activeJobs[jobID] = cancel
}

// UnregisterJob removes a completed or failed job from the active set.
func (a *AgentAPI) UnregisterJob(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.activeJobs, jobID)
}

func (a *AgentAPI) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *AgentAPI) handleExecute(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadSize))
	if err != nil {
		a.logger.Error("failed to read execute payload", "error", err)
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "failed to read request body"})
		return
	}

	// Verify Ed25519 signature before processing.
	if a.serverPubkey != nil {
		if err := a.verifySignature(body); err != nil {
			a.logger.Warn("signature verification failed", "error", err)
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid signature"})
			return
		}
	}

	var payload ExecutePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON payload"})
		return
	}

	if payload.Command == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "command is required"})
		return
	}
	if payload.Target == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "target is required"})
		return
	}

	a.logger.Info("execute command received",
		"command", payload.Command,
		"target", payload.Target,
		"agent_id", payload.AgentID,
	)

	if a.dispatcher == nil {
		a.logger.Error("job dispatcher not configured")
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "job dispatcher not configured"})
		return
	}

	jobID, err := a.dispatcher.StartJob(JobCommand{
		AgentID: payload.AgentID,
		Command: payload.Command,
		Target:  payload.Target,
		Params:  payload.Params,
	})
	if err != nil {
		a.logger.Error("failed to start job", "error", err)
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}

	a.logger.Info("job dispatched", "job_id", jobID, "command", payload.Command)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "accepted",
		"job_id": jobID,
	})
}

func (a *AgentAPI) handleAbortJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if jobID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "job id required"})
		return
	}

	a.mu.Lock()
	cancel, ok := a.activeJobs[jobID]
	a.mu.Unlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("job %q not found or already completed", jobID),
		})
		return
	}

	a.logger.Info("aborting job", "job_id", jobID)
	cancel()

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "cancelled",
		"job_id": jobID,
	})
}

// verifySignature extracts the "signature" field from the raw JSON,
// removes it to reconstruct the signed message, and verifies against
// the server's Ed25519 public key.
func (a *AgentAPI) verifySignature(rawBody []byte) error {
	// Extract the signature value.
	var envelope struct {
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return fmt.Errorf("parsing signature field: %w", err)
	}
	if envelope.Signature == "" {
		return fmt.Errorf("missing signature field")
	}

	sig, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil {
		return fmt.Errorf("decoding signature: %w", err)
	}

	// Reconstruct the signed message by removing the signature field.
	// Parse into a generic map, remove "signature", re-marshal deterministically.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &obj); err != nil {
		return fmt.Errorf("parsing payload for verification: %w", err)
	}
	delete(obj, "signature")

	message, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("re-marshaling signed content: %w", err)
	}

	if !ed25519.Verify(a.serverPubkey, message, sig) {
		return fmt.Errorf("ed25519 signature verification failed")
	}

	return nil
}

// loadEd25519PublicKey reads a raw 32-byte Ed25519 public key from a file.
// The file may be raw binary (32 bytes) or base64-encoded.
func loadEd25519PublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Raw 32-byte key.
	if len(data) == ed25519.PublicKeySize {
		return ed25519.PublicKey(data), nil
	}

	// Try base64 decoding (strip trailing whitespace).
	decoded, err := base64.StdEncoding.DecodeString(string(trimBytes(data)))
	if err != nil {
		return nil, fmt.Errorf("key file is neither raw 32-byte nor valid base64: %w", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("decoded key is %d bytes, expected %d", len(decoded), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(decoded), nil
}

func trimBytes(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
