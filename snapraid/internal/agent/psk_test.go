package agent

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pineappledr/vigil-addons/snapraid/internal/config"
)

func pskServer(psk string) *Server {
	cfg := &config.AgentConfig{}
	cfg.Hub.PSK = psk
	return &Server{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func callProtected(s *Server, auth string) (status int, ran bool) {
	h := s.requirePSK(func(w http.ResponseWriter, r *http.Request) { ran = true })
	req := httptest.NewRequest(http.MethodPost, "/api/execute", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code, ran
}

// The hole this closes: anyone on the LAN could POST {"command":"sync"}.
func TestRequirePSK_WithoutKeyIsRejected(t *testing.T) {
	code, ran := callProtected(pskServer("s3cret-psk-value"), "")
	if ran || code != http.StatusUnauthorized {
		t.Fatalf("no key: ran=%v code=%d, want rejected with 401", ran, code)
	}
}

func TestRequirePSK_WrongKeyIsRejected(t *testing.T) {
	code, ran := callProtected(pskServer("s3cret-psk-value"), "Bearer not-the-key")
	if ran || code != http.StatusUnauthorized {
		t.Fatalf("wrong key: ran=%v code=%d, want rejected with 401", ran, code)
	}
}

func TestRequirePSK_RightKeyPasses(t *testing.T) {
	if _, ran := callProtected(pskServer("s3cret-psk-value"), "Bearer s3cret-psk-value"); !ran {
		t.Fatal("right key must reach the handler")
	}
}

// A bare key without "Bearer " is a malformed header, not a pass.
func TestRequirePSK_KeyWithoutBearerPrefixIsRejected(t *testing.T) {
	if _, ran := callProtected(pskServer("s3cret-psk-value"), "s3cret-psk-value"); ran {
		t.Fatal("the key without the Bearer scheme must be rejected")
	}
}

// Standalone agents (no hub) keep working as before.
func TestRequirePSK_NoPSKConfiguredStaysOpen(t *testing.T) {
	if _, ran := callProtected(pskServer(""), ""); !ran {
		t.Fatal("an agent without a PSK must behave as before")
	}
}
