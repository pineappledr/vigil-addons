package agent

import (
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
)

// cronParser parses 5-field cron expressions (minute hour day month weekday).
// Descriptors (@daily, @every) are intentionally excluded: the scheduler
// registers entries via `"0 " + task.Schedule` so it can use the seconds
// slot, and that prefix is incompatible with descriptor form. Kept
// package-private; exposed only via validateCron.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// validateCron returns a user-facing error if expr is not a valid 5-field cron
// expression. Called by task/replication create+update handlers before
// persisting to the DB so the scheduler never sees a malformed schedule.
// Passes @every, @daily etc. through since the parser accepts descriptors.
func validateCron(expr string) error {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return fmt.Errorf("schedule is required")
	}
	if _, err := cronParser.Parse(trimmed); err != nil {
		return fmt.Errorf("invalid cron schedule %q: %w", trimmed, err)
	}
	return nil
}
