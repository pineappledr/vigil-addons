package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ReplicationResult holds the outcome of a local or remote send|receive pipeline.
type ReplicationResult struct {
	Command      string        `json:"command"`
	BytesSent    int64         `json:"bytes_sent"`
	Duration     time.Duration `json:"duration"`
	Incremental  bool          `json:"incremental"`
	SnapshotUsed string        `json:"snapshot_used"`
}

// RemoteTarget describes the destination of a remote replication over SSH.
//
// KeyPath, KnownHosts and PvPath are server-side filesystem paths — they are
// populated by the caller from the Engine's capability probe, not taken from
// user input.
type RemoteTarget struct {
	Host          string // hostname or IP, required
	Port          int    // 22 if zero
	User          string // SSH user, required
	KeyPath       string // private key file (populated from Engine.PrivateKeyPath)
	KnownHosts    string // known_hosts file (populated from Engine.KnownHostsPath)
	DestDataset   string // receive target on the remote host
	BandwidthKbps int    // 0 = no bandwidth cap
	PvPath        string // local pv binary; empty ⇒ bandwidth cap ignored
}

// sshPort returns the SSH port with a sensible default.
func (r RemoteTarget) sshPort() int {
	if r.Port == 0 {
		return 22
	}
	return r.Port
}

// --- Pure builders (unit-testable, no I/O) ---

// BuildSendCommand returns the CLI string for a zfs send.
// If baseSnap is empty, it's a full send; otherwise incremental (-i).
func BuildSendCommand(zfsPath, snap, baseSnap string) string {
	args := []string{zfsPath, "send"}
	if baseSnap != "" {
		args = append(args, "-i", baseSnap)
	}
	args = append(args, snap)
	return strings.Join(args, " ")
}

// BuildRecvCommand returns the CLI string for a zfs receive.
// force=true adds -F to rollback the destination to its most recent snapshot
// before receiving, which handles partial-receive cleanup.
func BuildRecvCommand(zfsPath, target string, force bool) string {
	args := []string{zfsPath, "receive"}
	if force {
		args = append(args, "-F")
	}
	args = append(args, target)
	return strings.Join(args, " ")
}

// BuildReplicationPipelineCommand returns the human-readable pipeline string
// for preview display.
func BuildReplicationPipelineCommand(zfsPath, snap, baseSnap, destTarget string) string {
	return BuildSendCommand(zfsPath, snap, baseSnap) + " | " + BuildRecvCommand(zfsPath, destTarget, true)
}

// BuildSSHArgs returns the invariant SSH flags used for every remote
// replication call: private key, known_hosts, batch mode, port, host-key
// accept-on-first-use, and target host.
//
// Callers append the remote command (e.g. "zfs receive -F dest") after these.
func BuildSSHArgs(tgt RemoteTarget) []string {
	args := []string{
		"-i", tgt.KeyPath,
		"-o", "UserKnownHostsFile=" + tgt.KnownHosts,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.sshPort()),
	}
	args = append(args, tgt.User+"@"+tgt.Host)
	return args
}

// BuildPvArgs returns the pv arguments for a given bandwidth cap (kbps).
// Returns nil if bandwidthKbps is 0 — callers should skip the pv stage then.
// -q suppresses pv's on-stderr progress output; -L enforces the rate limit.
func BuildPvArgs(bandwidthKbps int) []string {
	if bandwidthKbps <= 0 {
		return nil
	}
	return []string{"-q", "-L", strconv.Itoa(bandwidthKbps) + "k"}
}

// BuildRemoteReplicationPipeline returns the full preview string for a remote
// replication. Mirrors BuildReplicationPipelineCommand but over SSH, with an
// optional pv rate-limit stage.
//
// sshPath is the local ssh binary; pvPath may be empty (bandwidth cap is then
// silently dropped in the preview, matching what the runner does).
func BuildRemoteReplicationPipeline(zfsPath, sshPath, snap, baseSnap string, tgt RemoteTarget) string {
	send := BuildSendCommand(zfsPath, snap, baseSnap)

	remoteCmd := "zfs receive -F " + tgt.DestDataset
	ssh := sshPath + " " + strings.Join(BuildSSHArgs(tgt), " ") + " " + remoteCmd

	if tgt.BandwidthKbps > 0 && tgt.PvPath != "" {
		return send + " | " + tgt.PvPath + " " + strings.Join(BuildPvArgs(tgt.BandwidthKbps), " ") + " | " + ssh
	}
	return send + " | " + ssh
}

// FindCommonSnapshot returns the most recent snapshot name that exists on both
// the source and destination dataset. Returns "" if no common snapshot exists
// (indicating a full send is required).
func FindCommonSnapshot(srcSnapshots, dstSnapshots []SnapshotInfo) string {
	// Build set of destination snapshot names (just the @part).
	dstSet := make(map[string]struct{}, len(dstSnapshots))
	for _, s := range dstSnapshots {
		dstSet[s.SnapName] = struct{}{}
	}

	// Walk source snapshots from newest to oldest — we want the most recent common.
	// SnapshotInfo from ListSnapshots is sorted by creation ascending, so reverse.
	for i := len(srcSnapshots) - 1; i >= 0; i-- {
		if _, ok := dstSet[srcSnapshots[i].SnapName]; ok {
			return srcSnapshots[i].FullName
		}
	}
	return ""
}

// --- Streaming I/O ---

// SendReceiveLocal pipes `zfs send` stdout into `zfs receive` stdin on the
// same host. Both processes are tied to ctx for cancellation. Returns the
// number of bytes piped and any error.
func (e *Engine) SendReceiveLocal(ctx context.Context, snap, baseSnap, destTarget string) (*ReplicationResult, error) {
	start := time.Now()

	sendArgs := []string{"send"}
	incremental := baseSnap != ""
	if incremental {
		sendArgs = append(sendArgs, "-i", baseSnap)
	}
	sendArgs = append(sendArgs, snap)

	recvArgs := []string{"receive", "-F", destTarget}

	sendCmd := exec.CommandContext(ctx, e.zfsPath, sendArgs...) // #nosec G204,G702 -- zfsPath is set at startup from config
	recvCmd := exec.CommandContext(ctx, e.zfsPath, recvArgs...) // #nosec G204,G702 -- zfsPath is set at startup from config

	// Wire send stdout → counting reader → recv stdin.
	pr, pw := io.Pipe()
	sendCmd.Stdout = pw
	recvCmd.Stdin = pr

	var sendStderr, recvStderr bytes.Buffer
	sendCmd.Stderr = &sendStderr
	recvCmd.Stderr = &recvStderr

	cmdStr := BuildReplicationPipelineCommand(e.zfsPath, snap, baseSnap, destTarget)
	e.logger.Info("replication: starting local pipeline", "cmd", cmdStr)

	// Start receive first so it's ready to consume.
	if err := recvCmd.Start(); err != nil {
		return nil, fmt.Errorf("start zfs receive: %w", err)
	}

	if err := sendCmd.Start(); err != nil {
		// Kill the already-started receive.
		recvCmd.Process.Kill() //nolint:errcheck
		recvCmd.Wait()         //nolint:errcheck
		return nil, fmt.Errorf("start zfs send: %w", err)
	}

	// Wait for send to finish, then close the write side of the pipe
	// so receive sees EOF.
	sendErr := sendCmd.Wait()
	pw.Close()

	recvErr := recvCmd.Wait()

	duration := time.Since(start)

	if sendErr != nil {
		return nil, fmt.Errorf("zfs send failed: %w: %s", sendErr, strings.TrimSpace(sendStderr.String()))
	}
	if recvErr != nil {
		return nil, fmt.Errorf("zfs receive failed: %w: %s", recvErr, strings.TrimSpace(recvStderr.String()))
	}

	return &ReplicationResult{
		Command:      cmdStr,
		Duration:     duration,
		Incremental:  incremental,
		SnapshotUsed: snap,
	}, nil
}

// RemoteTargetFromTask builds a RemoteTarget from a scheduled-task row,
// filling the server-side paths from the engine's SSH key directory and pv
// probe. Returns an error if any required field is missing.
//
// The caller must have already validated that task is a remote replication
// (task_type='replication', replication_mode='remote').
func (e *Engine) RemoteTargetFromTask(destDataset, host, user string, port int, keyName string, bandwidthKbps int) (RemoteTarget, error) {
	if e.sshKeyDir == "" {
		return RemoteTarget{}, fmt.Errorf("ssh key directory not configured on agent")
	}
	if !sshKeyNameValid(keyName) {
		return RemoteTarget{}, fmt.Errorf("invalid ssh key name %q", keyName)
	}
	return RemoteTarget{
		Host:          host,
		Port:          port,
		User:          user,
		KeyPath:       PrivateKeyPath(e.sshKeyDir, keyName),
		KnownHosts:    KnownHostsPath(e.sshKeyDir),
		DestDataset:   destDataset,
		BandwidthKbps: bandwidthKbps,
		PvPath:        e.pvPath,
	}, nil
}

// SendReceiveRemote streams `zfs send` into a remote `zfs receive` over SSH,
// optionally rate-limited by pv in between. Both (or all three) processes share
// ctx — cancellation propagates to every child.
//
// Errors from any stage are combined into a single returned error with the
// stderr text for the stage that failed. Partial streams are not resumed:
// destinations are force-rolled back with `-F` on the next attempt.
func (e *Engine) SendReceiveRemote(ctx context.Context, snap, baseSnap string, tgt RemoteTarget) (*ReplicationResult, error) {
	if e.sshPath == "" {
		return nil, fmt.Errorf("%w: ssh not found on host", ErrCapabilityUnavailable)
	}
	if tgt.KeyPath == "" || tgt.KnownHosts == "" {
		return nil, fmt.Errorf("remote target missing key_path or known_hosts")
	}
	if tgt.Host == "" || tgt.User == "" || tgt.DestDataset == "" {
		return nil, fmt.Errorf("remote target missing host, user, or dest_dataset")
	}

	start := time.Now()

	// --- Build the three commands. ---
	sendArgs := []string{"send"}
	incremental := baseSnap != ""
	if incremental {
		sendArgs = append(sendArgs, "-i", baseSnap)
	}
	sendArgs = append(sendArgs, snap)

	sshArgs := append(BuildSSHArgs(tgt), "zfs", "receive", "-F", tgt.DestDataset)

	// #nosec G204 -- zfsPath and sshPath are set at engine startup; snap is a
	// validated ZFS snapshot name and tgt fields are either validated by the
	// HTTP handler (host/user) or derived from internal key storage (key/known).
	sendCmd := exec.CommandContext(ctx, e.zfsPath, sendArgs...)
	sshCmd := exec.CommandContext(ctx, e.sshPath, sshArgs...)

	var sendStderr, sshStderr, pvStderr bytes.Buffer
	sendCmd.Stderr = &sendStderr
	sshCmd.Stderr = &sshStderr

	// --- Wire the pipeline: send → [pv] → ssh ---
	bandwidth := tgt.BandwidthKbps > 0 && tgt.PvPath != ""
	var pvCmd *exec.Cmd
	var sshStdin, sendStdout *io.PipeWriter
	var sshStdinReader, sendStdoutReader *io.PipeReader

	if bandwidth {
		// sendCmd.Stdout → pvCmd.Stdin ; pvCmd.Stdout → sshCmd.Stdin
		sendStdoutReader, sendStdout = io.Pipe()
		sshStdinReader, sshStdin = io.Pipe()

		// #nosec G204 -- pvPath discovered via exec.LookPath at startup.
		pvCmd = exec.CommandContext(ctx, tgt.PvPath, BuildPvArgs(tgt.BandwidthKbps)...)
		pvCmd.Stdin = sendStdoutReader
		pvCmd.Stdout = sshStdin
		pvCmd.Stderr = &pvStderr

		sendCmd.Stdout = sendStdout
		sshCmd.Stdin = sshStdinReader
	} else {
		// Direct: sendCmd.Stdout → sshCmd.Stdin
		sendStdoutReader, sendStdout = io.Pipe()
		sendCmd.Stdout = sendStdout
		sshCmd.Stdin = sendStdoutReader
	}

	cmdStr := BuildRemoteReplicationPipeline(e.zfsPath, e.sshPath, snap, baseSnap, tgt)
	e.logger.Info("replication: starting remote pipeline",
		"cmd", cmdStr, "host", tgt.Host, "user", tgt.User, "dest", tgt.DestDataset, "bandwidth", bandwidth)

	// --- Start downstream-first so the upstream has somewhere to write. ---
	if err := sshCmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}
	if pvCmd != nil {
		if err := pvCmd.Start(); err != nil {
			sshCmd.Process.Kill() //nolint:errcheck
			sshCmd.Wait()         //nolint:errcheck
			return nil, fmt.Errorf("start pv: %w", err)
		}
	}
	if err := sendCmd.Start(); err != nil {
		if pvCmd != nil {
			pvCmd.Process.Kill() //nolint:errcheck
			pvCmd.Wait()         //nolint:errcheck
		}
		sshCmd.Process.Kill() //nolint:errcheck
		sshCmd.Wait()         //nolint:errcheck
		return nil, fmt.Errorf("start zfs send: %w", err)
	}

	// --- Wait upstream-to-downstream, closing pipes between stages. ---
	sendErr := sendCmd.Wait()
	sendStdout.Close() // signal EOF to pv (or directly to ssh)

	var pvErr error
	if pvCmd != nil {
		pvErr = pvCmd.Wait()
		sshStdin.Close() // signal EOF to ssh
	}

	sshErr := sshCmd.Wait()
	duration := time.Since(start)

	if sendErr != nil {
		return nil, fmt.Errorf("zfs send failed: %w: %s", sendErr, strings.TrimSpace(sendStderr.String()))
	}
	if pvErr != nil {
		return nil, fmt.Errorf("pv failed: %w: %s", pvErr, strings.TrimSpace(pvStderr.String()))
	}
	if sshErr != nil {
		return nil, fmt.Errorf("ssh/zfs receive failed: %w: %s", sshErr, strings.TrimSpace(sshStderr.String()))
	}

	return &ReplicationResult{
		Command:      cmdStr,
		Duration:     duration,
		Incremental:  incremental,
		SnapshotUsed: snap,
	}, nil
}

// ListRemoteSnapshotsForDataset is the SSH-over-wire analogue of
// ListSnapshotsForDataset: it runs `zfs list -t snapshot` on the remote host
// so the runner can resolve the most recent common snapshot for incremental
// sends. Returns an empty slice if the remote dataset has no snapshots yet
// (the typical first-run state).
func (e *Engine) ListRemoteSnapshotsForDataset(ctx context.Context, tgt RemoteTarget) ([]SnapshotInfo, error) {
	if e.sshPath == "" {
		return nil, fmt.Errorf("%w: ssh not found", ErrCapabilityUnavailable)
	}

	args := append(BuildSSHArgs(tgt),
		"zfs", "list", "-Hp", "-o", "name,creation,used,referenced",
		"-t", "snapshot", "-s", "creation", "-r", tgt.DestDataset,
	)

	// #nosec G204 -- sshPath is the probed binary; inputs validated by caller.
	cmd := exec.CommandContext(ctx, e.sshPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Remote may simply have no dataset yet on the very first run.
		msg := stderr.String()
		if strings.Contains(msg, "dataset does not exist") {
			return nil, nil
		}
		return nil, fmt.Errorf("list remote snapshots: %w: %s", err, strings.TrimSpace(msg))
	}

	var snapshots []SnapshotInfo
	for _, line := range splitLines(stdout.String()) {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		parts := strings.SplitN(fields[0], "@", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[0] != tgt.DestDataset {
			continue
		}
		snapshots = append(snapshots, SnapshotInfo{
			Dataset:    parts[0],
			SnapName:   parts[1],
			FullName:   fields[0],
			Creation:   fields[1],
			Used:       parseUint64(fields[2]),
			Referenced: parseUint64(fields[3]),
		})
	}
	return snapshots, nil
}

// TestRemoteConnection performs a read-only SSH probe against the remote host
// to verify three things: (1) the host key is trusted (or gets pinned now),
// (2) the private key is accepted, and (3) the remote user can see the
// destination dataset via `zfs list`.
//
// Returns the one-line dataset name on success; on failure, returns the SSH
// stderr so the UI can show the user what went wrong (auth, unknown host,
// permission denied on zfs list, etc.).
func (e *Engine) TestRemoteConnection(ctx context.Context, tgt RemoteTarget) (string, error) {
	if e.sshPath == "" {
		return "", fmt.Errorf("%w: ssh not found", ErrCapabilityUnavailable)
	}
	if tgt.KeyPath == "" || tgt.KnownHosts == "" {
		return "", fmt.Errorf("test-connection missing key_path or known_hosts")
	}
	if tgt.Host == "" || tgt.User == "" || tgt.DestDataset == "" {
		return "", fmt.Errorf("test-connection missing host, user, or dest_dataset")
	}

	args := append(BuildSSHArgs(tgt), "zfs", "list", "-H", "-o", "name", "-t", "filesystem", tgt.DestDataset)

	// #nosec G204 -- sshPath is the probed binary; all string inputs are either
	// agent-owned file paths or validated user input (see HTTP handler).
	cmd := exec.CommandContext(ctx, e.sshPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ListSnapshotsForDataset returns snapshots filtered to a specific dataset,
// sorted by creation ascending.
func (e *Engine) ListSnapshotsForDataset(ctx context.Context, dataset string) ([]SnapshotInfo, error) {
	out, err := e.runZFS(ctx, "list", "-Hp", "-o", "name,creation,used,referenced", "-t", "snapshot", "-s", "creation", "-r", dataset)
	if err != nil {
		// No snapshots is not an error — the dataset may simply have none.
		if strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, fmt.Errorf("list snapshots for %s: %w", dataset, err)
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
		// Only include direct snapshots of this dataset (not children).
		if parts[0] != dataset {
			continue
		}
		snapshots = append(snapshots, SnapshotInfo{
			Dataset:    parts[0],
			SnapName:   parts[1],
			FullName:   fields[0],
			Creation:   fields[1],
			Used:       parseUint64(fields[2]),
			Referenced: parseUint64(fields[3]),
		})
	}
	return snapshots, nil
}
