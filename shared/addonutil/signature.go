package addonutil

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// SignatureVerifier holds a parsed Ed25519 public key and provides
// command signature verification. If no key is configured (nil verifier),
// verification is skipped — acceptable for development only.
type SignatureVerifier struct {
	pubkey ed25519.PublicKey
}

// NewSignatureVerifier creates a verifier from a base64-encoded Ed25519
// public key string (the format exposed by the Vigil server's /api/server/pubkey).
// Returns nil if pubkeyB64 is empty (verification disabled).
func NewSignatureVerifier(pubkeyB64 string) (*SignatureVerifier, error) {
	pubkeyB64 = strings.TrimSpace(pubkeyB64)
	if pubkeyB64 == "" {
		return nil, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(pubkeyB64)
	if err != nil {
		return nil, fmt.Errorf("decode pubkey base64: %w", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pubkey is %d bytes, expected %d", len(decoded), ed25519.PublicKeySize)
	}

	return &SignatureVerifier{pubkey: ed25519.PublicKey(decoded)}, nil
}

// Enabled returns true if signature verification is active.
func (v *SignatureVerifier) Enabled() bool {
	return v != nil && v.pubkey != nil
}

// VerifyJSON verifies the Ed25519 signature embedded in a JSON payload.
// The payload must contain a "signature" field (base64-encoded).
// Verification reconstructs the signed message by removing "signature"
// from the JSON object and re-marshaling deterministically.
func (v *SignatureVerifier) VerifyJSON(rawBody []byte) error {
	if !v.Enabled() {
		return nil // verification disabled
	}

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
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &obj); err != nil {
		return fmt.Errorf("parsing payload for verification: %w", err)
	}
	delete(obj, "signature")

	message, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("re-marshaling signed content: %w", err)
	}

	if !ed25519.Verify(v.pubkey, message, sig) {
		return fmt.Errorf("ed25519 signature verification failed")
	}

	return nil
}
