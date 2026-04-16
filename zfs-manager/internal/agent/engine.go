package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrCapabilityUnavailable is returned by Engine methods when the host is
// missing an optional binary the operation depends on (e.g. ledctl for the
// drive-bay LED identification feature).
var ErrCapabilityUnavailable = errors.New("capability not available on this host")

// Engine wraps ZFS CLI commands.
type Engine struct {
	zpoolPath     string
	zfsPath       string
	ledctlPath    string
	sshPath       string
	sshKeygenPath string
	pvPath        string
	sshKeyDir     string
	logger        *slog.Logger
}

// Capabilities reports optional features the engine can perform based on which
// host binaries were found at startup. The set is fixed for the engine's
// lifetime — restart the agent if the host gains a new tool.
type Capabilities struct {
	LEDIdentify       bool `json:"led_identify"`
	RemoteReplication bool `json:"remote_replication"`
	BandwidthLimit    bool `json:"bandwidth_limit"`
}

// NewEngine creates an Engine with the given binary paths.
// If paths are empty, it auto-detects from $PATH.
// sshKeyDir is where Ed25519 keypairs and known_hosts are persisted for remote
// replication; if empty, remote replication is disabled even if ssh is present.
func NewEngine(zpoolPath, zfsPath, sshKeyDir string, logger *slog.Logger) *Engine {
	if zpoolPath == "" {
		zpoolPath = "zpool"
	}
	if zfsPath == "" {
		zfsPath = "zfs"
	}
	// Optional: ledctl powers the drive-bay LED identification feature. It is
	// not present on every host (no enclosure, no SES backplane, container
	// without /sys access), so a missing binary is not an error.
	ledctlPath, _ := exec.LookPath("ledctl")
	// Optional: ssh/ssh-keygen enable remote replication. pv enables the
	// bandwidth-limit option in the remote pipeline. All three are probed at
	// startup so the UI can grey out features the host cannot perform.
	sshPath, _ := exec.LookPath("ssh")
	sshKeygenPath, _ := exec.LookPath("ssh-keygen")
	pvPath, _ := exec.LookPath("pv")
	return &Engine{
		zpoolPath:     zpoolPath,
		zfsPath:       zfsPath,
		ledctlPath:    ledctlPath,
		sshPath:       sshPath,
		sshKeygenPath: sshKeygenPath,
		pvPath:        pvPath,
		sshKeyDir:     sshKeyDir,
		logger:        logger,
	}
}

// Capabilities returns the optional-feature flags probed at engine creation.
func (e *Engine) Capabilities() Capabilities {
	return Capabilities{
		LEDIdentify:       e.ledctlPath != "",
		RemoteReplication: e.sshPath != "" && e.sshKeygenPath != "" && e.sshKeyDir != "",
		BandwidthLimit:    e.pvPath != "",
	}
}

// --- Read Operations (telemetry) ---

// PoolInfo is the telemetry payload for a single pool.
type PoolInfo struct {
	Name        string     `json:"name"`
	Health      string     `json:"health"`
	Size        uint64     `json:"size"`
	Alloc       uint64     `json:"alloc"`
	Free        uint64     `json:"free"`
	Frag        int        `json:"frag"`
	Dedup       float64    `json:"dedup"`
	LastScrub   string     `json:"last_scrub"`
	ScrubStatus string     `json:"scrub_status"`
	Vdevs       []VdevInfo `json:"vdevs,omitempty"`
}

// VdevInfo describes a vdev in a pool topology.
type VdevInfo struct {
	Name   string     `json:"name"`
	Type   string     `json:"type"`
	Health string     `json:"health"`
	Read   uint64     `json:"read"`
	Write  uint64     `json:"write"`
	Cksum  uint64     `json:"cksum"`
	Disks  []DiskInfo `json:"disks,omitempty"`
}

// DiskInfo describes a disk within a vdev.
type DiskInfo struct {
	Name   string `json:"name"`
	Health string `json:"health"`
	Read   uint64 `json:"read"`
	Write  uint64 `json:"write"`
	Cksum  uint64 `json:"cksum"`
}

// DatasetInfo is the telemetry payload for a single dataset.
type DatasetInfo struct {
	Name       string `json:"name"`
	Used       uint64 `json:"used"`
	Avail      uint64 `json:"avail"`
	Refer      uint64 `json:"refer"`
	Compress   string `json:"compress"`
	RecordSize uint64 `json:"record_size"`
	Mountpoint string `json:"mountpoint"`
	Atime      string `json:"atime"`
	Sync       string `json:"sync"`
	Quota      string `json:"quota,omitempty"`
	Reserv     string `json:"reservation,omitempty"`
}

// SnapshotInfo is the telemetry payload for a single snapshot.
type SnapshotInfo struct {
	Dataset    string `json:"dataset"`
	SnapName   string `json:"snap_name"`
	FullName   string `json:"full_name"`
	Creation   string `json:"creation"`
	Used       uint64 `json:"used"`
	Referenced uint64 `json:"referenced"`
}

// ListPools returns pool information by parsing zpool list + zpool status.
func (e *Engine) ListPools(ctx context.Context) ([]PoolInfo, error) {
	// zpool list -Hp -o name,health,size,alloc,free,frag,dedup
	out, err := e.runZpool(ctx, "list", "-Hp", "-o", "name,health,size,alloc,free,frag,dedup")
	if err != nil {
		return nil, fmt.Errorf("zpool list: %w", err)
	}

	var pools []PoolInfo
	for _, line := range splitLines(out) {
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		pool := PoolInfo{
			Name:   fields[0],
			Health: fields[1],
			Size:   parseUint64(fields[2]),
			Alloc:  parseUint64(fields[3]),
			Free:   parseUint64(fields[4]),
			Frag:   parseInt(strings.TrimSuffix(fields[5], "%")),
			Dedup:  parseFloat64(strings.TrimSuffix(fields[6], "x")),
		}

		// Get scrub info
		scrubOut, err := e.runZpool(ctx, "status", "-p", pool.Name)
		if err == nil {
			pool.LastScrub, pool.ScrubStatus = parseScrubInfo(scrubOut)
			pool.Vdevs = parseVdevTopology(scrubOut)
		}

		pools = append(pools, pool)
	}
	return pools, nil
}

// ListDatasets returns all datasets with properties.
func (e *Engine) ListDatasets(ctx context.Context) ([]DatasetInfo, error) {
	out, err := e.runZFS(ctx, "list", "-Hp", "-o", "name,used,avail,refer,compression,recordsize,mountpoint,atime,sync", "-t", "filesystem,volume")
	if err != nil {
		return nil, fmt.Errorf("zfs list: %w", err)
	}

	var datasets []DatasetInfo
	for _, line := range splitLines(out) {
		fields := strings.Split(line, "\t")
		if len(fields) < 9 {
			continue
		}
		ds := DatasetInfo{
			Name:       fields[0],
			Used:       parseUint64(fields[1]),
			Avail:      parseUint64(fields[2]),
			Refer:      parseUint64(fields[3]),
			Compress:   fields[4],
			RecordSize: parseUint64(fields[5]),
			Mountpoint: fields[6],
			Atime:      fields[7],
			Sync:       fields[8],
		}

		// Get quota and reservation
		propsOut, err := e.runZFS(ctx, "get", "-Hp", "-o", "value", "quota,reservation", ds.Name)
		if err == nil {
			props := splitLines(propsOut)
			if len(props) >= 2 {
				if props[0] != "0" && props[0] != "none" {
					ds.Quota = props[0]
				}
				if props[1] != "0" && props[1] != "none" {
					ds.Reserv = props[1]
				}
			}
		}

		datasets = append(datasets, ds)
	}
	return datasets, nil
}

// ListSnapshots returns all snapshots.
func (e *Engine) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	out, err := e.runZFS(ctx, "list", "-Hp", "-o", "name,creation,used,referenced", "-t", "snapshot", "-s", "creation")
	if err != nil {
		return nil, fmt.Errorf("zfs list snapshots: %w", err)
	}

	var snapshots []SnapshotInfo
	for _, line := range splitLines(out) {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		parts := strings.SplitN(fields[0], "@", 2)
		if len(parts) != 2 {
			continue
		}
		snap := SnapshotInfo{
			Dataset:    parts[0],
			SnapName:   parts[1],
			FullName:   fields[0],
			Creation:   fields[1],
			Used:       parseUint64(fields[2]),
			Referenced: parseUint64(fields[3]),
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

// --- Write Operations (Phase 2) ---

// CommandResult holds the output from a write operation.
type CommandResult struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
}

// DatasetPreset defines recommended settings for different use cases.
type DatasetPreset struct {
	Name        string `json:"name"`
	RecordSize  string `json:"record_size"`
	Compression string `json:"compression"`
	Atime       string `json:"atime"`
	Sync        string `json:"sync"`
}

var DatasetPresets = map[string]DatasetPreset{
	"general": {Name: "General Purpose", RecordSize: "128K", Compression: "lz4", Atime: "off", Sync: "standard"},
	"media":   {Name: "Media Storage", RecordSize: "1M", Compression: "lz4", Atime: "off", Sync: "disabled"},
	"vm":      {Name: "VM/App Storage", RecordSize: "64K", Compression: "lz4", Atime: "off", Sync: "standard"},
	"db":      {Name: "Database", RecordSize: "16K", Compression: "lz4", Atime: "off", Sync: "always"},
}

// CreateDataset creates a new ZFS dataset with the given options.
func (e *Engine) CreateDataset(ctx context.Context, name string, props map[string]string) (*CommandResult, error) {
	args := []string{"create"}
	for k, v := range props {
		args = append(args, "-o", k+"="+v)
	}
	args = append(args, name)

	cmd := e.zfsPath + " " + strings.Join(args, " ")
	out, err := e.runZFS(ctx, args...)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// SetDatasetProperties modifies properties on an existing dataset.
func (e *Engine) SetDatasetProperties(ctx context.Context, name string, props map[string]string) (*CommandResult, error) {
	var results []string
	var lastCmd string
	for k, v := range props {
		args := []string{"set", k + "=" + v, name}
		lastCmd = e.zfsPath + " " + strings.Join(args, " ")
		out, err := e.runZFS(ctx, args...)
		if err != nil {
			return &CommandResult{Command: lastCmd, ExitCode: exitCode(err), Output: out, Error: err.Error()}, err
		}
		results = append(results, out)
	}
	return &CommandResult{Command: lastCmd, Output: strings.Join(results, "\n")}, nil
}

// DestroyDataset destroys a dataset and optionally its dependents.
func (e *Engine) DestroyDataset(ctx context.Context, name string, recursive bool) (*CommandResult, error) {
	args := []string{"destroy"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, name)

	cmd := e.zfsPath + " " + strings.Join(args, " ")
	out, err := e.runZFS(ctx, args...)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// CreateSnapshot takes a snapshot of the given dataset.
func (e *Engine) CreateSnapshot(ctx context.Context, dataset, snapName string, recursive bool) (*CommandResult, error) {
	fullName := dataset + "@" + snapName
	args := []string{"snapshot"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, fullName)

	cmd := e.zfsPath + " " + strings.Join(args, " ")
	out, err := e.runZFS(ctx, args...)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// DestroySnapshot deletes a snapshot.
func (e *Engine) DestroySnapshot(ctx context.Context, fullName string) (*CommandResult, error) {
	args := []string{"destroy", fullName}
	cmd := e.zfsPath + " " + strings.Join(args, " ")
	out, err := e.runZFS(ctx, args...)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// RollbackSnapshot rolls back a dataset to the given snapshot.
// depth: "latest" (no flags), "intermediate" (-r), "all" (-R).
func (e *Engine) RollbackSnapshot(ctx context.Context, fullName, depth string) (*CommandResult, error) {
	args := []string{"rollback"}
	switch depth {
	case "intermediate":
		args = append(args, "-r")
	case "all":
		args = append(args, "-R")
	}
	args = append(args, fullName)

	cmd := e.zfsPath + " " + strings.Join(args, " ")
	out, err := e.runZFS(ctx, args...)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// StartScrub initiates a scrub on the given pool.
func (e *Engine) StartScrub(ctx context.Context, pool string) (*CommandResult, error) {
	args := []string{"scrub", pool}
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

// PauseScrub pauses a running scrub.
func (e *Engine) PauseScrub(ctx context.Context, pool string) (*CommandResult, error) {
	args := []string{"scrub", "-p", pool}
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

// CancelScrub stops a running scrub.
func (e *Engine) CancelScrub(ctx context.Context, pool string) (*CommandResult, error) {
	args := []string{"scrub", "-s", pool}
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

// --- Phase 4: Disk & Pool Operations ---

// AvailableDisk describes a block device that is not part of any ZFS pool.
type AvailableDisk struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   uint64 `json:"size"`
	Model  string `json:"model"`
	Serial string `json:"serial"`
	Type   string `json:"type"` // "disk" or "part"
}

// ListAvailableDisks returns block devices not currently used by any ZFS pool.
// It cross-references lsblk output with disks found in zpool status.
func (e *Engine) ListAvailableDisks(ctx context.Context) ([]AvailableDisk, error) {
	// Get all disks/partitions currently in ZFS pools
	poolDisks := make(map[string]bool)
	pools, err := e.ListPools(ctx)
	if err == nil {
		for _, pool := range pools {
			for _, vdev := range pool.Vdevs {
				for _, disk := range vdev.Disks {
					// Normalize: strip /dev/ prefix and partition suffixes for matching
					poolDisks[disk.Name] = true
				}
				// Single-disk vdevs have the disk as the vdev name
				if vdev.Type == "disk" {
					poolDisks[vdev.Name] = true
				}
			}
		}
	}

	// List all block devices via lsblk
	out, err := e.run(ctx, "lsblk", "-Jbno", "NAME,PATH,SIZE,MODEL,SERIAL,TYPE")
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}

	type lsblkDevice struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Size   any    `json:"size"` // can be string or number
		Model  string `json:"model"`
		Serial string `json:"serial"`
		Type   string `json:"type"`
	}
	type lsblkOutput struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}

	var parsed lsblkOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}

	var available []AvailableDisk
	for _, dev := range parsed.BlockDevices {
		// Only include whole disks and partitions
		if dev.Type != "disk" && dev.Type != "part" {
			continue
		}
		// Skip if in a pool
		if poolDisks[dev.Name] {
			continue
		}
		var size uint64
		switch v := dev.Size.(type) {
		case float64:
			size = uint64(v)
		case string:
			size = parseUint64(v)
		}
		available = append(available, AvailableDisk{
			Name:   dev.Name,
			Path:   dev.Path,
			Size:   size,
			Model:  strings.TrimSpace(dev.Model),
			Serial: strings.TrimSpace(dev.Serial),
			Type:   dev.Type,
		})
	}
	return available, nil
}

// ReplaceDevice initiates a drive replacement in a pool.
func (e *Engine) ReplaceDevice(ctx context.Context, pool, oldDevice, newDevice string) (*CommandResult, error) {
	args := []string{"replace", pool, oldDevice, newDevice}
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

// AddVdev adds a new vdev to an existing pool.
// vdevType is "mirror", "raidz1", "raidz2", "raidz3", or "" (stripe).
func (e *Engine) AddVdev(ctx context.Context, pool, vdevType string, devices []string) (*CommandResult, error) {
	args := []string{"add", pool}
	if vdevType != "" && vdevType != "stripe" {
		args = append(args, vdevType)
	}
	args = append(args, devices...)

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

// OfflineDevice takes a device offline in a pool.
func (e *Engine) OfflineDevice(ctx context.Context, pool, device string) (*CommandResult, error) {
	args := []string{"offline", pool, device}
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

// OnlineDevice brings a device back online in a pool.
func (e *Engine) OnlineDevice(ctx context.Context, pool, device string) (*CommandResult, error) {
	args := []string{"online", pool, device}
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

// ClearErrors resets error counters for a pool or a specific device in a pool.
func (e *Engine) ClearErrors(ctx context.Context, pool, device string) (*CommandResult, error) {
	args := []string{"clear", pool}
	if device != "" {
		args = append(args, device)
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

// IdentifyDevice toggles the drive-bay identification LED for a device using
// ledctl. mode is "locate" (LED on) or "off" (LED back to normal).
// Returns ErrCapabilityUnavailable if ledctl was not found at engine startup.
func (e *Engine) IdentifyDevice(ctx context.Context, device, mode string) (*CommandResult, error) {
	if e.ledctlPath == "" {
		return nil, ErrCapabilityUnavailable
	}
	arg := buildLedctlArg(device, mode)
	cmd := e.ledctlPath + " " + arg
	out, err := e.run(ctx, e.ledctlPath, arg)
	result := &CommandResult{Command: cmd, Output: out}
	if err != nil {
		result.ExitCode = exitCode(err)
		result.Error = err.Error()
		return result, err
	}
	return result, nil
}

// --- Phase 4: Command Preview Builders ---

func BuildReplaceCommand(zpoolPath, pool, oldDevice, newDevice string) string {
	return zpoolPath + " replace " + pool + " " + oldDevice + " " + newDevice
}

func BuildAddVdevCommand(zpoolPath, pool, vdevType string, devices []string) string {
	args := []string{zpoolPath, "add", pool}
	if vdevType != "" && vdevType != "stripe" {
		args = append(args, vdevType)
	}
	args = append(args, devices...)
	return strings.Join(args, " ")
}

func BuildOfflineCommand(zpoolPath, pool, device string) string {
	return zpoolPath + " offline " + pool + " " + device
}

func BuildOnlineCommand(zpoolPath, pool, device string) string {
	return zpoolPath + " online " + pool + " " + device
}

func BuildClearCommand(zpoolPath, pool, device string) string {
	cmd := zpoolPath + " clear " + pool
	if device != "" {
		cmd += " " + device
	}
	return cmd
}

// BuildIdentifyCommand returns the ledctl invocation that toggles a drive-bay
// identification LED. mode is "locate" (light the bay) or "off" (return the
// bay to normal). Device names without a leading "/" are prefixed with /dev/.
func BuildIdentifyCommand(ledctlPath, device, mode string) string {
	return ledctlPath + " " + buildLedctlArg(device, mode)
}

// buildLedctlArg renders the ledctl single-argument form ("locate=/dev/sda" or
// "normal=/dev/sda") used by both BuildIdentifyCommand and IdentifyDevice.
func buildLedctlArg(device, mode string) string {
	verb := "locate"
	if mode == "off" {
		verb = "normal"
	}
	path := device
	if !strings.HasPrefix(path, "/") {
		path = "/dev/" + path
	}
	return verb + "=" + path
}

// --- Phase 4: Preview Warnings ---

// normalizeVdevType maps user-facing type strings into the internal form used
// by parseVdevTopology (which reports single-disk vdevs as "disk").
func normalizeVdevType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" || t == "stripe" {
		return "disk"
	}
	return t
}

// displayVdevType renders an internal vdev type in user-facing form.
func displayVdevType(t string) string {
	if t == "disk" {
		return "stripe"
	}
	return t
}

// CheckVdevTypeMatch compares a proposed vdev type against the data vdev types
// already present in a pool and returns human-readable warnings when the
// proposed type doesn't match. Special vdevs (cache, log, spare, special) are
// ignored because they are legitimately mixed with data vdevs.
//
// Returns nil (no warnings) when the pool has no data vdevs, when the proposed
// type matches at least one existing data vdev, or when existing data vdevs
// are already mixed (user is on their own in that case).
func CheckVdevTypeMatch(existing []VdevInfo, proposed string) []string {
	normalized := normalizeVdevType(proposed)

	seen := make(map[string]bool)
	for _, v := range existing {
		switch v.Type {
		case "cache", "log", "spare", "special":
			continue
		}
		seen[v.Type] = true
	}

	if len(seen) == 0 {
		return nil
	}
	if seen[normalized] {
		return nil
	}

	existingTypes := make([]string, 0, len(seen))
	for t := range seen {
		existingTypes = append(existingTypes, displayVdevType(t))
	}
	sort.Strings(existingTypes)

	return []string{
		fmt.Sprintf(
			"This pool currently uses %s vdev(s), but you're adding a %s vdev. Mixing vdev types is unusual and not recommended — losing any single vdev destroys the entire pool, so a weaker vdev reduces the whole pool's redundancy.",
			strings.Join(existingTypes, "/"),
			displayVdevType(normalized),
		),
	}
}

// CheckReplaceSize warns when the replacement device is smaller than the
// device being replaced. ZFS will refuse the replace in that case, so this
// surfaces the problem up-front before the user confirms.
//
// Accepts oldSize == 0 as "unknown" and skips the check — it's better to
// stay silent than to emit a misleading warning when lsblk couldn't resolve
// the device (e.g. a by-id path that no longer exists).
func CheckReplaceSize(oldSize, newSize uint64, oldDevice, newDevice string) []string {
	if oldSize == 0 || newSize == 0 {
		return nil
	}
	if newSize >= oldSize {
		return nil
	}
	return []string{
		fmt.Sprintf(
			"Replacement device %s is smaller than %s (%s vs %s). ZFS will refuse this replace. Use a device of equal or larger size.",
			newDevice, oldDevice,
			humanBytes(newSize), humanBytes(oldSize),
		),
	}
}

// humanBytes renders a byte count as a short human-readable string.
func humanBytes(n uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
		TiB = 1024 * GiB
		PiB = 1024 * TiB
	)
	switch {
	case n >= PiB:
		return fmt.Sprintf("%.2f PiB", float64(n)/float64(PiB))
	case n >= TiB:
		return fmt.Sprintf("%.2f TiB", float64(n)/float64(TiB))
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// DeviceSize returns the raw byte size of a block device. Accepts short names
// ("sda") or absolute paths ("/dev/sda", "/dev/disk/by-id/..."). Returns 0
// without an error when lsblk fails to resolve the device — callers use that
// as "unknown" rather than treating it as fatal, since warnings must be
// best-effort.
func (e *Engine) DeviceSize(ctx context.Context, device string) uint64 {
	if device == "" {
		return 0
	}
	path := device
	if !strings.HasPrefix(path, "/") {
		path = "/dev/" + path
	}
	out, err := e.run(ctx, "lsblk", "-bndo", "SIZE", path)
	if err != nil {
		e.logger.Debug("lsblk size lookup failed", "device", device, "error", err)
		return 0
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0
	}
	return parseUint64(fields[0])
}

// --- Helpers ---

func (e *Engine) runZpool(ctx context.Context, args ...string) (string, error) {
	return e.run(ctx, e.zpoolPath, args...)
}

func (e *Engine) runZFS(ctx context.Context, args ...string) (string, error) {
	return e.run(ctx, e.zfsPath, args...)
}

func (e *Engine) run(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204,G702 -- bin is set at startup from config, not user input
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	e.logger.Debug("executing", "cmd", bin, "args", args)

	if err := cmd.Run(); err != nil {
		combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		return combined, fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, combined)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func splitLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func parseUint64(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "-" || s == "" {
		return 0
	}
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func parseInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "-" || s == "" {
		return 0
	}
	v, _ := strconv.Atoi(s)
	return v
}

func parseFloat64(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "-" || s == "" {
		return 1.0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func exitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

// parseScrubInfo extracts last scrub date and current scrub status from zpool status output.
func parseScrubInfo(statusOutput string) (lastScrub, scrubStatus string) {
	lastScrub = "none"
	scrubStatus = "none"
	for _, line := range strings.Split(statusOutput, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "scan:") {
			rest := strings.TrimPrefix(line, "scan:")
			rest = strings.TrimSpace(rest)
			if strings.Contains(rest, "scrub repaired") {
				// "scrub repaired 0B in 01:23:45 with 0 errors on Sun Apr  6 02:00:01 2026"
				if idx := strings.Index(rest, " on "); idx != -1 {
					dateStr := strings.TrimSpace(rest[idx+4:])
					if t, err := time.Parse("Mon Jan  2 15:04:05 2006", dateStr); err == nil {
						lastScrub = t.UTC().Format(time.RFC3339)
					} else if t, err := time.Parse("Mon Jan 2 15:04:05 2006", dateStr); err == nil {
						lastScrub = t.UTC().Format(time.RFC3339)
					} else {
						lastScrub = dateStr
					}
				}
				scrubStatus = "completed"
			} else if strings.Contains(rest, "scrub in progress") {
				scrubStatus = "in_progress"
				// Try to extract progress percentage
				if idx := strings.Index(rest, "done"); idx != -1 {
					parts := strings.Fields(rest[:idx])
					if len(parts) > 0 {
						scrubStatus = "in_progress (" + parts[len(parts)-1] + " done)"
					}
				}
			} else if strings.Contains(rest, "scrub canceled") {
				scrubStatus = "canceled"
			} else if strings.Contains(rest, "scrub paused") {
				scrubStatus = "paused"
			} else if strings.Contains(rest, "resilver") {
				scrubStatus = "resilvering"
			} else if strings.Contains(rest, "none requested") {
				scrubStatus = "none"
			}
		}
	}
	return
}

// parseVdevTopology parses zpool status output into vdev/disk tree.
func parseVdevTopology(statusOutput string) []VdevInfo {
	lines := strings.Split(statusOutput, "\n")

	// Find the config section
	configStart := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "config:" {
			configStart = i + 1
			break
		}
	}
	if configStart < 0 {
		return nil
	}

	// Skip the header line (NAME STATE READ WRITE CKSUM)
	headerIdx := -1
	for i := configStart; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "NAME") {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return nil
	}

	var vdevs []VdevInfo
	var currentVdev *VdevInfo

	for i := headerIdx + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break // end of config section
		}
		if strings.HasPrefix(trimmed, "errors:") {
			break
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}

		// Determine indentation level
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		name := fields[0]
		health := fields[1]
		var read, write, cksum uint64
		if len(fields) >= 5 {
			read = parseUint64(fields[2])
			write = parseUint64(fields[3])
			cksum = parseUint64(fields[4])
		}

		// Pool-level line (indent ~2-4): skip, it's the pool name
		// Vdev-level line (indent ~4-6): mirror-0, raidz1-0, etc.
		// Disk-level line (indent ~8+): sda, sdb, etc.
		if indent <= 4 {
			// Could be pool name — skip if matches a known pool pattern
			// (first entry after header is always the pool name)
			if currentVdev == nil && !isVdevType(name) {
				continue // pool name line
			}
		}

		if isVdevType(name) || (indent <= 8 && currentVdev == nil) {
			if currentVdev != nil {
				vdevs = append(vdevs, *currentVdev)
			}
			vdevType := "disk" // single disk, no redundancy
			if strings.HasPrefix(name, "mirror") {
				vdevType = "mirror"
			} else if strings.HasPrefix(name, "raidz") {
				vdevType = name[:strings.IndexByte(name, '-')]
			} else if strings.HasPrefix(name, "spare") {
				vdevType = "spare"
			} else if strings.HasPrefix(name, "cache") {
				vdevType = "cache"
			} else if strings.HasPrefix(name, "log") {
				vdevType = "log"
			}
			currentVdev = &VdevInfo{
				Name:   name,
				Type:   vdevType,
				Health: health,
				Read:   read,
				Write:  write,
				Cksum:  cksum,
			}
		} else if currentVdev != nil {
			currentVdev.Disks = append(currentVdev.Disks, DiskInfo{
				Name:   name,
				Health: health,
				Read:   read,
				Write:  write,
				Cksum:  cksum,
			})
		} else {
			// Disk directly under pool (stripe / single disk)
			vdev := VdevInfo{
				Name:   name,
				Type:   "disk",
				Health: health,
				Read:   read,
				Write:  write,
				Cksum:  cksum,
			}
			vdevs = append(vdevs, vdev)
		}
	}

	if currentVdev != nil {
		vdevs = append(vdevs, *currentVdev)
	}
	return vdevs
}

func isVdevType(name string) bool {
	prefixes := []string{"mirror", "raidz", "spare", "cache", "log", "special"}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// BuildCommand returns the CLI command string that would be executed, for preview.
func BuildCreateDatasetCommand(zfsPath, name string, props map[string]string) string {
	args := []string{zfsPath, "create"}
	for k, v := range props {
		args = append(args, "-o", k+"="+v)
	}
	args = append(args, name)
	return strings.Join(args, " ")
}

func BuildDestroyDatasetCommand(zfsPath, name string, recursive bool) string {
	args := []string{zfsPath, "destroy"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, name)
	return strings.Join(args, " ")
}

func BuildSnapshotCommand(zfsPath, dataset, snapName string, recursive bool) string {
	args := []string{zfsPath, "snapshot"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, dataset+"@"+snapName)
	return strings.Join(args, " ")
}

func BuildRollbackCommand(zfsPath, fullName, depth string) string {
	args := []string{zfsPath, "rollback"}
	switch depth {
	case "intermediate":
		args = append(args, "-r")
	case "all":
		args = append(args, "-R")
	}
	args = append(args, fullName)
	return strings.Join(args, " ")
}

func BuildScrubCommand(zpoolPath, pool, action string) string {
	args := []string{zpoolPath, "scrub"}
	switch action {
	case "pause":
		args = append(args, "-p")
	case "cancel":
		args = append(args, "-s")
	}
	args = append(args, pool)
	return strings.Join(args, " ")
}
