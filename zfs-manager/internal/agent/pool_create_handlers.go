package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pineappledr/vigil-addons/shared/addonutil"
)

// createPoolRequest is the wire body for POST /api/pool/create. It accepts
// two alternative shapes so the frontend form and the admin JSON-poster can
// both reach the same handler:
//
//  1. `vdevs: [...]` — full structured spec. Preferred when callers need
//     multi-vdev layouts (e.g. mirror-of-mirrors + log + cache).
//  2. `data_type`, `data_devices`, `log_devices`, `cache_devices`,
//     `spare_devices`, `special_type`, `special_devices` — the flat form
//     used by the manifest wizard. Only one data vdev shape is supported
//     through this path; admins with more complex needs use (1).
//
// When both are provided, `vdevs` wins — the flat fields are ignored. This
// gives the JSON-poster a clean escape hatch without breaking the wizard.
type createPoolRequest struct {
	Name  string         `json:"name"`
	Vdevs []PoolVdevSpec `json:"vdevs,omitempty"`

	DataType      string   `json:"data_type,omitempty"`
	DataDevices   []string `json:"data_devices,omitempty"`
	LogType       string   `json:"log_type,omitempty"`
	LogDevices    []string `json:"log_devices,omitempty"`
	CacheDevices  []string `json:"cache_devices,omitempty"`
	SpareDevices  []string `json:"spare_devices,omitempty"`
	SpecialType   string   `json:"special_type,omitempty"`
	SpecialDevice []string `json:"special_devices,omitempty"`

	Ashift     int               `json:"ashift,omitempty"`
	Mountpoint string            `json:"mountpoint,omitempty"`
	PoolProps  map[string]string `json:"pool_props,omitempty"`
	FsProps    map[string]string `json:"fs_props,omitempty"`

	// Convenience aliases — the manifest form can set them as flat fields
	// instead of dropping into pool_props/fs_props maps.
	Compression string `json:"compression,omitempty"`
	Atime       string `json:"atime,omitempty"`
	Recordsize  string `json:"recordsize,omitempty"`
	Autotrim    string `json:"autotrim,omitempty"`

	Force   bool   `json:"force,omitempty"`
	Confirm string `json:"confirm,omitempty"`
}

// toSpec collapses the flat wizard fields into a CreatePoolSpec. If the
// request already carries `vdevs`, the flat fields are ignored — the handler
// uses that path verbatim.
func (r *createPoolRequest) toSpec() CreatePoolSpec {
	spec := CreatePoolSpec{
		Name:       strings.TrimSpace(r.Name),
		Ashift:     r.Ashift,
		Mountpoint: strings.TrimSpace(r.Mountpoint),
		PoolProps:  cloneStringMap(r.PoolProps),
		FsProps:    cloneStringMap(r.FsProps),
		Force:      r.Force,
		Confirm:    r.Confirm,
	}
	if spec.FsProps == nil {
		spec.FsProps = map[string]string{}
	}
	if spec.PoolProps == nil {
		spec.PoolProps = map[string]string{}
	}
	// Pull convenience aliases into the respective maps. The structured
	// maps still win if both were provided — form fields get out of sync
	// with advanced edits and the explicit map represents intent.
	if r.Compression != "" {
		if _, set := spec.FsProps["compression"]; !set {
			spec.FsProps["compression"] = r.Compression
		}
	}
	if r.Atime != "" {
		if _, set := spec.FsProps["atime"]; !set {
			spec.FsProps["atime"] = r.Atime
		}
	}
	if r.Recordsize != "" {
		if _, set := spec.FsProps["recordsize"]; !set {
			spec.FsProps["recordsize"] = r.Recordsize
		}
	}
	if r.Autotrim != "" {
		if _, set := spec.PoolProps["autotrim"]; !set {
			spec.PoolProps["autotrim"] = r.Autotrim
		}
	}

	if len(r.Vdevs) > 0 {
		spec.Vdevs = r.Vdevs
		return spec
	}

	// Flat wizard path — build vdevs in a canonical order so
	// BuildCreatePoolCommand emits a stable string.
	dataType := r.DataType
	if dataType == "" {
		dataType = "stripe"
	}
	if len(r.DataDevices) > 0 {
		spec.Vdevs = append(spec.Vdevs, PoolVdevSpec{
			Role: VdevRoleData, Type: dataType, Devices: trimDeviceList(r.DataDevices),
		})
	}
	if len(r.LogDevices) > 0 {
		t := r.LogType
		if t == "" {
			t = "stripe"
		}
		spec.Vdevs = append(spec.Vdevs, PoolVdevSpec{
			Role: VdevRoleLog, Type: t, Devices: trimDeviceList(r.LogDevices),
		})
	}
	if len(r.CacheDevices) > 0 {
		spec.Vdevs = append(spec.Vdevs, PoolVdevSpec{
			Role: VdevRoleCache, Type: "stripe", Devices: trimDeviceList(r.CacheDevices),
		})
	}
	if len(r.SpareDevices) > 0 {
		spec.Vdevs = append(spec.Vdevs, PoolVdevSpec{
			Role: VdevRoleSpare, Type: "stripe", Devices: trimDeviceList(r.SpareDevices),
		})
	}
	if len(r.SpecialDevice) > 0 {
		t := r.SpecialType
		if t == "" {
			t = "mirror"
		}
		spec.Vdevs = append(spec.Vdevs, PoolVdevSpec{
			Role: VdevRoleSpecial, Type: t, Devices: trimDeviceList(r.SpecialDevice),
		})
	}
	return spec
}

// handleCreatePool validates the spec, optionally renders a preview (when
// the caller sets preview=1), and otherwise executes `zpool create`. Force
// creation requires confirm=<pool name> so the user can't accidentally
// reuse disks that are in another system's partition table.
func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	var req createPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	spec := req.toSpec()
	if err := ValidatePoolSpec(spec); err != nil {
		addonutil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Red-tier gate: force is how users reuse disks that still carry foreign
	// zfs/lvm/md labels. It's also how they overwrite a pool they didn't
	// realise was still imported on another host. Type-to-confirm.
	if spec.Force && spec.Confirm != spec.Name {
		addonutil.WriteError(w, http.StatusBadRequest, "force create is red-tier: send confirm=<pool name> to apply")
		return
	}

	if r.URL.Query().Get("preview") == "1" {
		addonutil.WriteJSON(w, http.StatusOK, map[string]string{
			"command": BuildCreatePoolCommand(s.engine.zpoolPath, spec),
		})
		return
	}

	s.logger.Info("creating pool", "name", spec.Name, "vdevs", len(spec.Vdevs), "force", spec.Force)
	s.logOp("zpool-create", "info", fmt.Sprintf("creating pool %s with %d vdevs (force=%t)", spec.Name, len(spec.Vdevs), spec.Force))

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	result, err := s.engine.CreatePool(ctx, spec)
	if err != nil {
		s.logger.Error("create pool failed", "name", spec.Name, "error", err)
		s.logOp("zpool-create", "error", "create pool "+spec.Name+" failed: "+err.Error())
		if result == nil {
			result = &CommandResult{Command: BuildCreatePoolCommand(s.engine.zpoolPath, spec), Error: err.Error()}
		}
		addonutil.WriteJSON(w, http.StatusInternalServerError, result)
		return
	}

	s.logOp("zpool-create", "info", "pool "+spec.Name+" created")
	go s.refreshAndFlush()
	addonutil.WriteJSON(w, http.StatusOK, result)
}

// trimDeviceList drops blank entries and trims whitespace. Multi-select form
// inputs sometimes send "" for unset slots which would otherwise trip the
// per-device validator with a "empty device at slot N" error.
func trimDeviceList(in []string) []string {
	out := in[:0]
	for _, d := range in {
		d = strings.TrimSpace(d)
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// cloneStringMap returns a shallow copy so the incoming request's map isn't
// aliased into the spec — the handler mutates spec.FsProps/PoolProps when
// folding in the convenience-alias fields.
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
