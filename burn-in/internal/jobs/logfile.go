package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pineapple/vigil-addons/burn-in/internal/drive"
)

// JobLogFile writes structured burn-in progress and events to a persistent
// log file on disk, following the Spearfoot convention of per-drive log files:
//
//	burnin-{model}_{serial}_{timestamp}.log
//
// This allows operators to review test results long after the job completes.
type JobLogFile struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// NewJobLogFile creates or truncates a log file for the given job and drive.
// The log directory is created if it does not exist.
func NewJobLogFile(logDir, jobID string, info *drive.DriveInfo) (*JobLogFile, error) {
	if logDir == "" {
		return nil, nil
	}

	if err := os.MkdirAll(filepath.Clean(logDir), 0o750); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	ts := time.Now().UTC().Format("20060102T150405")
	filename := fmt.Sprintf("burnin-%s_%s_%s.log", sanitizeField(info.Model), sanitizeField(info.Serial), ts)
	path := filepath.Join(logDir, filename)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}

	return &JobLogFile{file: f, path: path}, nil
}

// Path returns the absolute path to the log file.
func (l *JobLogFile) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// WriteHeader writes the initial runtime information block, mirroring the
// Spearfoot disk-burnin.sh log header format.
func (l *JobLogFile) WriteHeader(jobID string, info *drive.DriveInfo) {
	if l == nil {
		return
	}
	l.writeSeparator("Started burn-in")
	l.writeLine("Job ID:                 %s", jobID)
	l.writeLine("Drive:                  %s", info.Path)
	if info.ByIDPath != "" {
		l.writeLine("By-ID Path:             %s", info.ByIDPath)
	}
	l.writeLine("Drive Model:            %s", info.Model)
	l.writeLine("Serial Number:          %s", info.Serial)
	l.writeLine("Disk Type:              %s", string(info.Rotation))
	l.writeLine("Capacity:               %d bytes", info.CapacityBytes)
	l.writeLine("")
}

// WritePhase writes a phase transition header.
func (l *JobLogFile) WritePhase(phase string) {
	if l == nil {
		return
	}
	l.writeSeparator(phase)
}

// WriteLog writes a timestamped log line with severity.
func (l *JobLogFile) WriteLog(severity, format string, args ...any) {
	if l == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	l.writeLine("[%s] [%s] %s", now, severity, msg)
}

// WriteResult writes the final result summary.
func (l *JobLogFile) WriteResult(passed bool, failReason string, badblockErrs int, duration time.Duration) {
	if l == nil {
		return
	}
	l.writeSeparator("Finished burn-in")
	if passed {
		l.writeLine("Result:                 PASSED")
	} else {
		l.writeLine("Result:                 FAILED")
		l.writeLine("Reason:                 %s", failReason)
	}
	l.writeLine("Bad block errors:       %d", badblockErrs)
	l.writeLine("Total duration:         %s", duration.Round(time.Second))
}

// Close flushes and closes the log file.
func (l *JobLogFile) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

func (l *JobLogFile) writeSeparator(title string) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.file, "+-----------------------------------------------------------------------------\n")
	fmt.Fprintf(l.file, "+ %s: %s\n", title, now)
	fmt.Fprintf(l.file, "+-----------------------------------------------------------------------------\n")
}

func (l *JobLogFile) writeLine(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.file, format+"\n", args...)
}

// sanitizeField replaces spaces and slashes with underscores for safe filenames.
func sanitizeField(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}
