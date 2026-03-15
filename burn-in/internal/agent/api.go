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

	"github.com/pineappledr/vigil-addons/shared/addonutil"
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

// JobHistoryFunc returns all job records (active + completed) as a JSON-marshalable slice.
type JobHistoryFunc func() any

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
	historyFn    JobHistoryFunc

	mu         sync.Mutex
	activeJobs map[string]JobCancelFunc
}

// SetJobDispatcher injects the job manager after construction to break
// the circular dependency (JobManager needs AgentAPI for lifecycle callbacks,
// AgentAPI needs JobManager for dispatch).
func (a *AgentAPI) SetJobDispatcher(d JobDispatcher) {
	a.dispatcher = d
}

// SetJobHistoryFunc injects the function used to retrieve all job records.
func (a *AgentAPI) SetJobHistoryFunc(fn JobHistoryFunc) {
	a.historyFn = fn
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
	mux.HandleFunc("GET /api/jobs/history", a.handleJobHistory)
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
	addonutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *AgentAPI) handleJobHistory(w http.ResponseWriter, _ *http.Request) {
	if a.historyFn == nil {
		a.logger.Error("job history provider not configured")
		addonutil.WriteJSON(w, http.StatusInternalServerError, addonutil.ErrorResponse{Error: "job history not available"})
		return
	}

	records := a.historyFn()
	a.logger.Info("job history requested")
	addonutil.WriteJSON(w, http.StatusOK, records)
}

func (a *AgentAPI) handleExecute(w http.ResponseWriter, r *http.Request) {
	a.logger.Info("execute endpoint hit",
		"method", r.Method,
		"remote_addr", r.RemoteAddr,
		"content_length", r.ContentLength,
	)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadSize))
	if err != nil {
		a.logger.Error("failed to read execute payload", "error", err)
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "failed to read request body"})
		return
	}

	a.logger.Info("execute payload received", "body_size", len(body))

	// Verify Ed25519 signature before processing.
	if a.serverPubkey != nil {
		a.logger.Info("verifying ed25519 signature")
		if err := a.verifySignature(body); err != nil {
			a.logger.Warn("signature verification failed",
				"error", err,
				"pubkey_len", len(a.serverPubkey),
			)
			addonutil.WriteJSON(w, http.StatusUnauthorized, addonutil.ErrorResponse{Error: "invalid signature"})
			return
		}
		a.logger.Info("signature verification passed")
	} else {
		a.logger.Warn("signature verification skipped (no pubkey configured)")
	}

	var payload ExecutePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		a.logger.Error("execute payload JSON parse failed", "error", err)
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "invalid JSON payload"})
		return
	}

	if payload.Command == "" {
		a.logger.Warn("execute rejected: missing command field")
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "command is required"})
		return
	}
	if payload.Target == "" {
		a.logger.Warn("execute rejected: missing target field")
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "target is required"})
		return
	}

	a.logger.Info("execute command parsed",
		"command", payload.Command,
		"target", payload.Target,
		"agent_id", payload.AgentID,
		"has_params", len(payload.Params) > 0,
		"has_signature", payload.Signature != "",
	)

	if a.dispatcher == nil {
		a.logger.Error("job dispatcher not configured — cannot process command")
		addonutil.WriteJSON(w, http.StatusInternalServerError, addonutil.ErrorResponse{Error: "job dispatcher not configured"})
		return
	}

	a.logger.Info("dispatching job to manager",
		"command", payload.Command,
		"target", payload.Target,
	)

	jobID, err := a.dispatcher.StartJob(JobCommand{
		AgentID: payload.AgentID,
		Command: payload.Command,
		Target:  payload.Target,
		Params:  payload.Params,
	})
	if err != nil {
		a.logger.Error("job dispatch failed",
			"command", payload.Command,
			"target", payload.Target,
			"error", err,
		)
		addonutil.WriteJSON(w, http.StatusConflict, addonutil.ErrorResponse{Error: err.Error()})
		return
	}

	a.logger.Info("job accepted and running",
		"job_id", jobID,
		"command", payload.Command,
		"target", payload.Target,
	)
	addonutil.WriteJSON(w, http.StatusAccepted, map[string]string{
		"status": "accepted",
		"job_id": jobID,
	})
}

func (a *AgentAPI) handleAbortJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if jobID == "" {
		addonutil.WriteJSON(w, http.StatusBadRequest, addonutil.ErrorResponse{Error: "job id required"})
		return
	}

	a.mu.Lock()
	cancel, ok := a.activeJobs[jobID]
	a.mu.Unlock()

	if !ok {
		addonutil.WriteJSON(w, http.StatusNotFound, addonutil.ErrorResponse{
			Error: fmt.Sprintf("job %q not found or already completed", jobID),
		})
		return
	}

	a.logger.Info("aborting job", "job_id", jobID)
	cancel()

	addonutil.WriteJSON(w, http.StatusOK, map[string]string{
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

