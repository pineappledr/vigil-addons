package engine

import (
	"context"
	"fmt"
)

// Touch executes `snapraid touch` to normalize file timestamps.
func (e *Engine) Touch(ctx context.Context) (*TouchReport, error) {
	result, err := e.runCommand(ctx, "touch")
	if err != nil {
		return nil, fmt.Errorf("touch: %w", err)
	}

	return &TouchReport{
		ExitCode: result.ExitCode,
		Output:   result.CombinedOutput(),
	}, nil
}
