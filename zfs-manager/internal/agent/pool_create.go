package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// PoolVdevRole tags what a vdev is used for in the pool layout. Data vdevs
// hold the striped redundant payload; log/cache/spare/special vdevs are the
// auxiliary ZFS concepts. Keeping role as a separate field (instead of
// overloading `type`) lets the validator apply role-specific rules like
// "cache cannot be raidz".
type PoolVdevRole string

const (
	VdevRoleData    PoolVdevRole = "data"
	VdevRoleLog     PoolVdevRole = "log"
	VdevRoleCache   PoolVdevRole = "cache"
	VdevRoleSpare   PoolVdevRole = "spare"
	VdevRoleSpecial PoolVdevRole = "special"
)

// PoolVdevSpec describes one vdev in a pool layout. `Type` is the redundancy
// shape ("stripe", "mirror", "raidz1", "raidz2", "raidz3"); the meaning of
// `stripe` varies with role (see validatePoolVdev for role-specific rules).
type PoolVdevSpec struct {
	Role    PoolVdevRole `json:"role"`
	Type    string       `json:"type"`
	Devices []string     `json:"devices"`
}

// CreatePoolSpec is the full wire contract for POST /api/pool/create. Every
// knob the wizard can set lives on this struct — the handler converts the
// flat form body into this shape before validation.
type CreatePoolSpec struct {
	Name       string            `json:"name"`
	Vdevs      []PoolVdevSpec    `json:"vdevs"`
	Ashift     int               `json:"ashift,omitempty"`
	Mountpoint string            `json:"mountpoint,omitempty"`
	PoolProps  map[string]string `json:"pool_props,omitempty"`
	FsProps    map[string]string `json:"fs_props,omitempty"`
	Force      bool              `json:"force,omitempty"`
	Confirm    string            `json:"confirm,omitempty"`
}

// poolNamePattern matches pool names zpool accepts. We're stricter than
// zpool proper — disallowing dots and leading digits — because homelab UIs
// get enough foot-guns from device paths alone.
var poolNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// reservedPoolNames are tokens zpool treats specially in `zpool create` —
// accepting them as a pool name produces bizarre parse errors several layers
// down. Easier to reject up front.
var reservedPoolNames = map[string]bool{
	"mirror":  true,
	"raidz":   true,
	"raidz1":  true,
	"raidz2":  true,
	"raidz3":  true,
	"spare":   true,
	"log":     true,
	"cache":   true,
	"special": true,
	"dedup":   true,
	"draid":   true,
}

// validVdevTypes is the set of zpool-create vdev shapes we let through.
// Anything not in this set would be passed to zpool unvalidated, which is
// how people create pools whose real layout surprises them.
var validVdevTypes = map[string]bool{
	"stripe": true,
	"mirror": true,
	"raidz1": true,
	"raidz2": true,
	"raidz3": true,
}

// minDevicesByType is the lower bound of disks per vdev for each type. These
// are hard zpool requirements — fewer devices fails at the kernel level but
// the error messages are cryptic so we reject here first.
var minDevicesByType = map[string]int{
	"stripe": 1,
	"mirror": 2,
	"raidz1": 3,
	"raidz2": 4,
	"raidz3": 5,
}

// ValidatePoolSpec is the gate every pool creation passes through. Returns
// the *first* error so UI form validation can highlight one field at a time,
// but the checks are ordered deliberately — name first, then per-vdev, then
// cross-vdev collision.
func ValidatePoolSpec(spec CreatePoolSpec) error {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return fmt.Errorf("pool name is required")
	}
	if !poolNamePattern.MatchString(name) {
		return fmt.Errorf("pool name %q must start with a letter and contain only letters, digits, underscores, or hyphens", name)
	}
	if reservedPoolNames[strings.ToLower(name)] {
		return fmt.Errorf("pool name %q is a reserved zpool keyword", name)
	}

	if len(spec.Vdevs) == 0 {
		return fmt.Errorf("at least one vdev is required")
	}
	hasData := false
	for i, v := range spec.Vdevs {
		if err := validatePoolVdev(v, i); err != nil {
			return err
		}
		if v.Role == VdevRoleData {
			hasData = true
		}
	}
	if !hasData {
		return fmt.Errorf("at least one data vdev is required")
	}

	// Collision check — if the user accidentally assigns the same device to
	// two vdevs zpool would fail loudly, but the error mentions the kernel
	// block device, not our role names. Fail here so the UI can point at
	// the user's own form.
	seen := make(map[string]string, 16)
	for _, v := range spec.Vdevs {
		for _, d := range v.Devices {
			key := strings.TrimSpace(d)
			if prev, ok := seen[key]; ok {
				return fmt.Errorf("device %q appears in both %s and %s vdevs", key, prev, v.Role)
			}
			seen[key] = string(v.Role)
		}
	}

	if spec.Ashift != 0 && (spec.Ashift < 9 || spec.Ashift > 16) {
		return fmt.Errorf("ashift %d is outside the accepted 9..16 range", spec.Ashift)
	}
	if spec.Mountpoint != "" {
		if !strings.HasPrefix(spec.Mountpoint, "/") {
			return fmt.Errorf("mountpoint %q must be an absolute path", spec.Mountpoint)
		}
		if strings.ContainsAny(spec.Mountpoint, "\n\r\x00") {
			return fmt.Errorf("mountpoint contains control characters")
		}
	}
	return nil
}

// validatePoolVdev applies role-specific rules. Cache/spare vdevs have
// restricted shapes — zpool rejects e.g. "cache raidz1 …" and the error
// surfaces as "invalid vdev specification" which tells the user nothing.
func validatePoolVdev(v PoolVdevSpec, idx int) error {
	switch v.Role {
	case VdevRoleData, VdevRoleLog, VdevRoleSpecial:
		// All three accept stripe/mirror/raidz* though raidz is unusual
		// for log/special. We still allow it — documented support and
		// valid for some admins' disaster-recovery designs.
	case VdevRoleCache, VdevRoleSpare:
		// zpool create treats cache and spare as a flat list. Only "stripe"
		// (single disks, no redundancy shape prefix) is legal.
		if v.Type != "" && v.Type != "stripe" {
			return fmt.Errorf("vdev %d (%s) must use type 'stripe' — zpool does not accept %s for this role", idx, v.Role, v.Type)
		}
	default:
		return fmt.Errorf("vdev %d has unknown role %q", idx, v.Role)
	}

	t := v.Type
	if t == "" {
		t = "stripe"
	}
	if !validVdevTypes[t] {
		return fmt.Errorf("vdev %d has unsupported type %q", idx, v.Type)
	}
	if len(v.Devices) == 0 {
		return fmt.Errorf("vdev %d (%s/%s) has no devices", idx, v.Role, t)
	}
	if min, ok := minDevicesByType[t]; ok && len(v.Devices) < min {
		return fmt.Errorf("vdev %d (%s/%s) needs at least %d devices, got %d", idx, v.Role, t, min, len(v.Devices))
	}
	for j, d := range v.Devices {
		d = strings.TrimSpace(d)
		if d == "" {
			return fmt.Errorf("vdev %d (%s/%s) has an empty device at slot %d", idx, v.Role, t, j)
		}
		if strings.ContainsAny(d, "\n\r\x00 \t") {
			return fmt.Errorf("vdev %d device %q contains whitespace or control characters", idx, d)
		}
	}
	return nil
}

// BuildCreatePoolCommand renders the exact `zpool create` invocation that
// CreatePool will run. The preview endpoint returns this string so the
// confirmation dialog can show the command before we execute it — users
// routinely copy-paste to verify against their own notes.
//
// The output ordering is deterministic for a given spec:
//
//	zpool create [-f] [-o pool_prop]* [-O fs_prop]* [-m mountpoint] <name> \
//	  <data vdevs> [log …] [cache …] [spare …] [special …]
func BuildCreatePoolCommand(zpoolPath string, spec CreatePoolSpec) string {
	if zpoolPath == "" {
		zpoolPath = "zpool"
	}
	args := []string{zpoolPath, "create"}
	if spec.Force {
		args = append(args, "-f")
	}
	if spec.Ashift > 0 {
		args = append(args, "-o", fmt.Sprintf("ashift=%d", spec.Ashift))
	}
	for _, k := range sortedKeys(spec.PoolProps) {
		args = append(args, "-o", k+"="+spec.PoolProps[k])
	}
	for _, k := range sortedKeys(spec.FsProps) {
		args = append(args, "-O", k+"="+spec.FsProps[k])
	}
	if spec.Mountpoint != "" {
		args = append(args, "-m", spec.Mountpoint)
	}
	args = append(args, spec.Name)

	// Emit data vdevs first, then auxiliary roles in a fixed order so the
	// command string is stable across identical specs.
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleData)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleLog)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleCache)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleSpare)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleSpecial)

	return strings.Join(args, " ")
}

// appendVdevArgs appends the argv for every vdev with matching role. For
// role=data the "data" keyword is implicit (zpool expects the vdevs straight
// after the pool name); for all other roles we emit the keyword before each
// vdev. A stripe vdev emits its devices without a type prefix (zpool treats
// a bare list as stripe).
func appendVdevArgs(args []string, vdevs []PoolVdevSpec, role PoolVdevRole) []string {
	for _, v := range vdevs {
		if v.Role != role {
			continue
		}
		if role != VdevRoleData {
			args = append(args, string(role))
		}
		t := v.Type
		if t == "" {
			t = "stripe"
		}
		if t != "stripe" {
			args = append(args, t)
		}
		args = append(args, v.Devices...)
	}
	return args
}

// sortedKeys returns map keys sorted so the command-string output is stable.
// Avoids pulling in "sort" just for this one call — same pattern used by
// properties.go upstream.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// CreatePool builds the zpool-create command from spec and executes it.
// Validation is delegated to ValidatePoolSpec — callers should invoke it
// first so the confirm dialog can surface form errors without executing.
func (e *Engine) CreatePool(ctx context.Context, spec CreatePoolSpec) (*CommandResult, error) {
	if err := ValidatePoolSpec(spec); err != nil {
		return nil, err
	}
	cmd := BuildCreatePoolCommand(e.zpoolPath, spec)
	args := buildCreatePoolArgs(spec)
	out, err := e.runZpool(ctx, args...)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// buildCreatePoolArgs mirrors BuildCreatePoolCommand but returns the raw argv
// slice. Keeping two functions avoids splitting the formatted string to
// reconstruct argv (which would mishandle whitespace in property values).
func buildCreatePoolArgs(spec CreatePoolSpec) []string {
	args := []string{"create"}
	if spec.Force {
		args = append(args, "-f")
	}
	if spec.Ashift > 0 {
		args = append(args, "-o", fmt.Sprintf("ashift=%d", spec.Ashift))
	}
	for _, k := range sortedKeys(spec.PoolProps) {
		args = append(args, "-o", k+"="+spec.PoolProps[k])
	}
	for _, k := range sortedKeys(spec.FsProps) {
		args = append(args, "-O", k+"="+spec.FsProps[k])
	}
	if spec.Mountpoint != "" {
		args = append(args, "-m", spec.Mountpoint)
	}
	args = append(args, spec.Name)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleData)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleLog)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleCache)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleSpare)
	args = appendVdevArgs(args, spec.Vdevs, VdevRoleSpecial)
	return args
}
