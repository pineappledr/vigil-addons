package agent

import (
	"context"
	"fmt"
	"strings"
)

// ImportablePoolEntry is one pool discovered by `zpool import` (no pool name
// argument). The subset returned here is everything the UI needs to render a
// candidate row and build a safe import command.
//
// `zpool import` with no -H flag emits human-readable paragraphs, not a table,
// so we parse key:value prefixes line by line. Fields we don't recognise are
// ignored — future zpool releases may add new ones and silently dropping them
// is safer than failing the listing for lines we don't understand.
type ImportablePoolEntry struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	State   string `json:"state"`
	Status  string `json:"status,omitempty"`
	Action  string `json:"action,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// ListImportablePools runs `zpool import` (discovery mode) and parses the
// resulting paragraphs into a list. `dirs` is optional; when non-empty each
// entry is passed as a `-d <dir>` flag so callers can restrict the search
// (e.g. to /dev/disk/by-id for stable naming).
//
// Returns an empty slice — not an error — when no pools are importable. `zpool
// import` exits 0 with an empty body in that case, and bubbling up "exit 1"
// would mask the far-more-common "clean machine, nothing to import" state.
func (e *Engine) ListImportablePools(ctx context.Context, dirs []string) ([]ImportablePoolEntry, error) {
	args := []string{"import"}
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		args = append(args, "-d", d)
	}
	out, err := e.runZpool(ctx, args...)
	// Some zpool versions return exit 1 when no pools are available. The
	// stdout "no pools available to import" sentinel is the authoritative
	// signal; downgrade that case to an empty list.
	if err != nil && strings.Contains(strings.ToLower(out), "no pools available") {
		return []ImportablePoolEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("zpool import: %w", err)
	}
	return parseImportablePools(out), nil
}

// ImportPoolOptions are the knobs the hub can pass to ImportPool. Each field
// maps 1:1 to a zpool-import flag; the zero value is the safe default.
type ImportPoolOptions struct {
	// NewName renames the pool on import. Useful when importing a pool whose
	// name collides with an already-imported pool.
	NewName string
	// Altroot mounts the pool under a temporary root, skipping the normal
	// mountpoints. Callers typically use this for recovery on foreign pools.
	Altroot string
	// Force imports a pool that is listed as potentially in use by another
	// host. This is the classic foot-gun — two hosts importing the same pool
	// corrupts it. The handler requires type-to-confirm before setting this.
	Force bool
	// ReadOnly imports in read-only mode. Safer for inspection but the pool
	// is not usable for writes until re-imported.
	ReadOnly bool
	// Dirs narrows device discovery to the listed directories.
	Dirs []string
}

// ImportPool imports a pool by name or numeric ID. The caller identifies the
// pool via `name` (which zpool-import accepts as either a name string or a
// GUID). Options translate directly to command-line flags.
func (e *Engine) ImportPool(ctx context.Context, name string, opts ImportPoolOptions) (*CommandResult, error) {
	if name == "" {
		return nil, fmt.Errorf("pool name or id required")
	}
	args := []string{"import"}
	if opts.Force {
		args = append(args, "-f")
	}
	if opts.ReadOnly {
		args = append(args, "-o", "readonly=on")
	}
	if opts.Altroot != "" {
		args = append(args, "-R", opts.Altroot)
	}
	for _, d := range opts.Dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		args = append(args, "-d", d)
	}
	args = append(args, name)
	if opts.NewName != "" {
		args = append(args, opts.NewName)
	}

	cmd := e.zpoolPath + " " + strings.Join(args, " ")
	out, err := e.runZpool(ctx, args...)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// ExportPool unmounts and releases a pool. After a successful export the pool
// vanishes from `zpool list` until re-imported. `force` sets -f which tears
// down active I/O — required when background processes hold the pool open.
func (e *Engine) ExportPool(ctx context.Context, name string, force bool) (*CommandResult, error) {
	if name == "" {
		return nil, fmt.Errorf("pool name required")
	}
	args := []string{"export"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, name)

	cmd := e.zpoolPath + " " + strings.Join(args, " ")
	out, err := e.runZpool(ctx, args...)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// parseImportablePools turns the human-readable output of `zpool import` into
// structured entries. Each pool block starts with a line whose left-trimmed
// prefix is "pool:". Lines we don't recognise (including the multi-line
// "config:" device tree) are deliberately dropped — the UI shows the device
// layout after the user selects a pool and clicks import, at which point the
// agent runs a scoped `zpool import <name>` to get the detailed view.
func parseImportablePools(out string) []ImportablePoolEntry {
	pools := make([]ImportablePoolEntry, 0)
	var cur *ImportablePoolEntry
	flush := func() {
		if cur != nil && cur.Name != "" {
			pools = append(pools, *cur)
		}
		cur = nil
	}
	for _, raw := range splitLines(out) {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		key, val, ok := splitImportKV(line)
		if !ok {
			// Non key:value lines are the body of "config:" / "status:" / "action:"
			// — we don't need them for the summary listing.
			continue
		}
		switch key {
		case "pool":
			flush()
			cur = &ImportablePoolEntry{Name: val}
		case "id":
			if cur != nil {
				cur.ID = val
			}
		case "state":
			if cur != nil {
				cur.State = val
			}
		case "status":
			if cur != nil {
				cur.Status = val
			}
		case "action":
			if cur != nil {
				cur.Action = val
			}
		case "comment":
			if cur != nil {
				cur.Comment = val
			}
		}
	}
	flush()
	return pools
}

// splitImportKV splits a "key: value" line that may have extra whitespace.
// Returns ok=false when the line doesn't look like a key:value pair so the
// caller can skip it without a nil-check.
func splitImportKV(line string) (string, string, bool) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	key := strings.ToLower(strings.TrimSpace(line[:idx]))
	val := strings.TrimSpace(line[idx+1:])
	// "config:" has no inline value — skip.
	if val == "" {
		return "", "", false
	}
	// Only whitelisted short keys are treated as header fields. This keeps
	// wordy lines like "action: The pool can be imported..." in scope while
	// rejecting accidental key-like prefixes in device configs (e.g.
	// "sda1  ONLINE" lines don't contain a colon, so they're already dropped).
	switch key {
	case "pool", "id", "state", "status", "action", "comment":
		return key, val, true
	}
	return "", "", false
}
