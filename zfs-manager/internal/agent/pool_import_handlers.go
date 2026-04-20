package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
)

// handleListImportablePools returns the candidate pools visible to `zpool
// import`. Optional `?dirs=/dev/disk/by-id,/dev/disk/by-partuuid` narrows the
// device discovery scope; empty means use the zpool default.
func (s *Server) handleListImportablePools(w http.ResponseWriter, r *http.Request) {
	dirs := splitCSVParam(r.URL.Query().Get("dirs"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	entries, err := s.engine.ListImportablePools(ctx, dirs)
	if err != nil {
		s.logger.Warn("list importable pools failed", "error", err)
		addonutil.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	addonutil.WriteJSON(w, http.StatusOK, entries)
}

// importPoolRequest mirrors ImportPoolOptions on the wire. Name is the pool
// name OR numeric id; Confirm must equal Name for any force-import so the hub
// cannot accidentally auto-force a pool that's in use on another host.
type importPoolRequest struct {
	Name     string   `json:"name"`
	NewName  string   `json:"new_name,omitempty"`
	Altroot  string   `json:"altroot,omitempty"`
	Force    bool     `json:"force,omitempty"`
	ReadOnly bool     `json:"readonly,omitempty"`
	Dirs     []string `json:"dirs,omitempty"`
	Confirm  string   `json:"confirm,omitempty"`
}

// handleImportPool imports a discovered pool. Force imports are the only
// meaningful red-tier case (a force import of a pool currently in use by
// another host is how two-node corruption happens), so we gate only the -f
// path with a type-to-confirm check.
func (s *Server) handleImportPool(w http.ResponseWriter, r *http.Request) {
	var req importPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Force && req.Confirm != req.Name {
		addonutil.WriteError(w, http.StatusBadRequest, "force import is red-tier: send confirm=<pool name or id> to apply")
		return
	}

	s.logger.Info("importing pool", "name", req.Name, "new_name", req.NewName, "force", req.Force, "readonly", req.ReadOnly)
	s.logOp("zpool-import", "info", "importing pool "+req.Name)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	result, err := s.engine.ImportPool(ctx, req.Name, ImportPoolOptions{
		NewName:  req.NewName,
		Altroot:  req.Altroot,
		Force:    req.Force,
		ReadOnly: req.ReadOnly,
		Dirs:     req.Dirs,
	})
	if err != nil {
		s.logger.Error("import pool failed", "name", req.Name, "error", err)
		s.logOp("zpool-import", "error", "import "+req.Name+" failed: "+err.Error())
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}
	s.logOp("zpool-import", "info", "pool "+req.Name+" imported")

	// A successful import reshapes the pool/dataset/snapshot inventory, so
	// flush the collector before responding. Clients polling telemetry will
	// see the new pool on their very next tick.
	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

// exportPoolRequest is the body of POST /api/pool/export. Export is always
// disruptive (the pool disappears from the system until re-imported), so we
// require Confirm==Pool unconditionally — there is no "mild" export.
type exportPoolRequest struct {
	Pool    string `json:"pool"`
	Force   bool   `json:"force,omitempty"`
	Confirm string `json:"confirm,omitempty"`
}

// handleExportPool exports (unmounts + releases) a pool. The UI always issues
// a type-to-confirm dialog because re-import is a manual step and users
// commonly conflate export with "temporarily detach".
func (s *Server) handleExportPool(w http.ResponseWriter, r *http.Request) {
	var req exportPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Pool = strings.TrimSpace(req.Pool)
	if req.Pool == "" {
		addonutil.WriteError(w, http.StatusBadRequest, "pool is required")
		return
	}
	if req.Confirm != req.Pool {
		addonutil.WriteError(w, http.StatusBadRequest, "export is red-tier: send confirm=<pool name> to apply")
		return
	}

	s.logger.Info("exporting pool", "pool", req.Pool, "force", req.Force)
	s.logOp("zpool-export", "info", "exporting pool "+req.Pool)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	result, err := s.engine.ExportPool(ctx, req.Pool, req.Force)
	if err != nil {
		s.logger.Error("export pool failed", "pool", req.Pool, "error", err)
		s.logOp("zpool-export", "error", "export "+req.Pool+" failed: "+err.Error())
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}
	s.logOp("zpool-export", "info", "pool "+req.Pool+" exported")

	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}
