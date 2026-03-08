package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var (
	// Matches summary lines like: "   123 added" or "   456 removed"
	reDiffCounter = regexp.MustCompile(`^\s*(\d+)\s+(added|removed|updated|moved|copied|restored)$`)

	// Matches verbose per-file lines like: "add path/to/file"
	reDiffEntry = regexp.MustCompile(`^(add|remove|update|move|copy|restore)\s+(.+)$`)
)

// Diff executes `snapraid diff` and parses the output.
func (e *Engine) Diff(ctx context.Context) (*DiffReport, error) {
	result, err := e.runCommand(ctx, "diff")
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}

	report := parseDiff(result.Stdout)
	report.Output = result.CombinedOutput()
	return report, nil
}

func parseDiff(output string) *DiffReport {
	r := &DiffReport{}

	for _, line := range strings.Split(output, "\n") {
		if m := reDiffCounter.FindStringSubmatch(line); m != nil {
			count := atoi(m[1])
			switch m[2] {
			case "added":
				r.Added = count
			case "removed":
				r.Removed = count
			case "updated":
				r.Updated = count
			case "moved":
				r.Moved = count
			case "copied":
				r.Copied = count
			case "restored":
				r.Restored = count
			}
		}
		if m := reDiffEntry.FindStringSubmatch(line); m != nil {
			r.FileDetails = append(r.FileDetails, DiffEntry{
				Change: m[1],
				Path:   m[2],
			})
		}
	}

	r.HasChanges = r.Added > 0 || r.Removed > 0 || r.Updated > 0 ||
		r.Moved > 0 || r.Copied > 0 || r.Restored > 0

	return r
}
