package hub

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Agents now require the PSK: a hub that forwarded commands without it would
// break every button in the UI the day the agent is upgraded.
func TestRouterPost_SendsThePSK(t *testing.T) {
	var got string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer agent.Close()

	cr := NewCommandRouter(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cr.SetPSKSource(func() string { return "hub-psk" })
	resp, err := cr.post(agent.URL+"/api/execute", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer hub-psk" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer hub-psk")
	}
}
