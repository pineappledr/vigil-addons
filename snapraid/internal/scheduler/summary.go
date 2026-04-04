package scheduler

import (
	"strings"
)

// FormatCommandSummary extracts a concise human-readable summary from a
// snapraid command's raw output. Each command type has its own extractor.
func FormatCommandSummary(jobType, output string) string {
	if output == "" {
		return ""
	}
	switch jobType {
	case "scrub", "sync", "fix":
		return formatOperationSummary(output)
	case "status":
		return formatStatusLines(output)
	case "diff":
		return formatDiffLines(output)
	case "smart":
		return formatSmartLines(output)
	case "touch":
		return formatTouchLines(output)
	default:
		return ""
	}
}

// formatOperationSummary extracts the completion line and result from
// scrub/sync/fix output.
//
// Example output:
//
//	100% completed, 2744411 MB accessed in 0:50
//	Everything OK
func formatOperationSummary(output string) string {
	var lines []string
	for _, line := range splitOutput(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "% completed") && strings.Contains(line, " accessed in ") {
			lines = append(lines, cleanCompletionLine(line))
			continue
		}
		if strings.EqualFold(line, "everything ok") {
			lines = append(lines, "Everything OK")
			continue
		}
		if strings.HasPrefix(line, "DANGER") || strings.HasPrefix(line, "WARNING") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// cleanCompletionLine trims the trailing ETA/progress artifacts that can
// appear when \r-separated progress lines are captured alongside the final
// completion summary.
//
//	"100% completed, 2744411 MB accessed in 0:50    %, 0:00 ETA"
//	→ "100% completed, 2744411 MB accessed in 0:50"
func cleanCompletionLine(line string) string {
	const marker = " accessed in "
	idx := strings.Index(line, marker)
	if idx < 0 {
		return strings.TrimSpace(line)
	}
	after := strings.TrimSpace(line[idx+len(marker):])
	fields := strings.Fields(after)
	if len(fields) == 0 {
		return strings.TrimSpace(line)
	}
	return line[:idx] + marker + fields[0]
}

// formatStatusLines extracts the natural-language summary lines from
// snapraid status output, skipping the disk table and preamble.
//
// Example output:
//
//	The oldest block was scrubbed 11 days ago, the median 11, the newest 0.
//	No sync is in progress.
//	92% of the array is not scrubbed.
//	No file has a zero sub-second timestamp.
//	No rehash is in progress or needed.
//	No error detected.
func formatStatusLines(output string) string {
	var lines []string
	for _, line := range splitOutput(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "The ") ||
			strings.HasPrefix(line, "No ") ||
			strings.Contains(line, "% of the array") ||
			strings.Contains(line, "in progress") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// formatDiffLines extracts the file change counts from snapraid diff output,
// skipping the loading/scanning preamble.
//
// Example output:
//
//	267794 equal
//	     0 added
//	     0 removed
//	     0 updated
//	     0 moved
//	     0 copied
//	     0 restored
//	No differences
func formatDiffLines(output string) string {
	var lines []string
	for _, line := range splitOutput(output) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Loading state") ||
			strings.HasPrefix(trimmed, "Using ") ||
			trimmed == "Comparing..." ||
			strings.HasPrefix(trimmed, "Initializing") ||
			strings.HasPrefix(trimmed, "Saving state") {
			continue
		}
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, "\n")
}

// formatSmartLines extracts the overall failure probability from snapraid
// smart output.
//
// Example output:
//
//	Probability that at least one disk is going to fail in the next year is 0%.
func formatSmartLines(output string) string {
	var lines []string
	for _, line := range splitOutput(output) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Probability that at least one disk") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// formatTouchLines extracts the result from snapraid touch output, skipping
// the loading/scanning preamble.
func formatTouchLines(output string) string {
	var lines []string
	for _, line := range splitOutput(output) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Loading state") ||
			strings.HasPrefix(trimmed, "Using ") ||
			strings.HasPrefix(trimmed, "Initializing") ||
			strings.HasPrefix(trimmed, "Saving state") ||
			strings.HasPrefix(trimmed, "Scanning disk") {
			continue
		}
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, "\n")
}

// splitOutput splits snapraid output on both \n and \r, handling the mixed
// line endings produced by streaming progress updates.
func splitOutput(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r'
	})
}
