package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
)

func (s *Server) handlePropertyCatalog(w http.ResponseWriter, _ *http.Request) {
	addonutil.WriteJSON(w, http.StatusOK, BuildPropertyCatalog())
}

// handleGetDatasetProperties returns current values for the resolved dataset.
// The caller identifies the dataset via ?name=; an optional ?keys=k1,k2
// narrows the read, otherwise the full editable catalog is returned.
func (s *Server) handleGetDatasetProperties(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}
	keys := splitCSVParam(r.URL.Query().Get("keys"))

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := s.engine.GetDatasetProperties(ctx, name, keys)
	if err != nil {
		s.logger.Warn("get dataset properties failed", "name", name, "error", err)
		addonutil.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, rows)
}

// handleGetPoolProperties mirrors handleGetDatasetProperties for pools.
func (s *Server) handleGetPoolProperties(w http.ResponseWriter, r *http.Request) {
	pool := strings.TrimSpace(r.URL.Query().Get("pool"))
	if pool == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool query parameter is required")
		return
	}
	keys := splitCSVParam(r.URL.Query().Get("keys"))

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := s.engine.GetPoolProperties(ctx, pool, keys)
	if err != nil {
		s.logger.Warn("get pool properties failed", "pool", pool, "error", err)
		addonutil.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, rows)
}

type setPoolPropertiesRequest struct {
	Pool       string            `json:"pool"`
	Properties map[string]string `json:"properties"`
	Confirm    string            `json:"confirm,omitempty"`
}

// handleSetPoolProperties validates the requested changes against the catalog
// and applies them with `zpool set`. Red-tier changes (e.g. failmode=panic)
// require the caller to send confirm=<pool> matching the pool name; green and
// yellow tier changes pass through without extra friction.
func (s *Server) handleSetPoolProperties(w http.ResponseWriter, r *http.Request) {
	var req setPoolPropertiesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" || len(req.Properties) == 0 {
		addonutil.WriteError(w, http.StatusBadRequest, "pool and properties are required")
		return
	}

	catalog := BuildPropertyCatalog()
	for k, v := range req.Properties {
		if err := ValidateProperty(catalog, ScopePool, k, v); err != nil {
			addonutil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	tier := ClassifyChange(catalog, ScopePool, req.Properties)
	if tier == TierRed && req.Confirm != req.Pool {
		addonutil.WriteError(w, http.StatusBadRequest, "red-tier change: send confirm=<pool name> to apply")
		return
	}

	s.logger.Info("setting pool properties", "pool", req.Pool, "tier", tier, "props", req.Properties)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.engine.SetPoolProperties(ctx, req.Pool, req.Properties)
	if err != nil {
		s.logger.Error("set pool properties failed", "pool", req.Pool, "error", err)
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

// splitCSVParam splits a comma-separated query string, trimming whitespace
// around each entry and dropping empty fields. Used by both get-properties
// handlers.
func splitCSVParam(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
