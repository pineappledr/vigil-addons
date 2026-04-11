package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ReplicationResult holds the outcome of a local send|receive pipeline.
type ReplicationResult struct {
	Command      string        `json:"command"`
	BytesSent    int64         `json:"bytes_sent"`
	Duration     time.Duration `json:"duration"`
	Incremental  bool          `json:"incremental"`
	SnapshotUsed string        `json:"snapshot_used"`
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

	sendCmd := exec.CommandContext(ctx, e.zfsPath, sendArgs...) // #nosec G204 — zfsPath is set at startup from config
	recvCmd := exec.CommandContext(ctx, e.zfsPath, recvArgs...) // #nosec G204 — zfsPath is set at startup from config

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
