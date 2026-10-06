package steps

import (
	"context"
	"errors"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
)

// Step is one unit of an operation.
type Step struct {
	// ID is stable and kebab-case (e.g. "db-migrate"): users pass it to
	// --skip-step and it appears in events and error messages.
	ID string
	// Title is the human description shown while the step runs.
	Title string
	// Check inspects real state and reports whether the step is already
	// done, so a re-run skips it. A nil Check means the step always applies.
	// Check must not change anything.
	Check func(ctx context.Context) (done bool, err error)
	// Apply does the work, reporting through sink (Progress, Log, Warning).
	// It must be safe to re-run after a failure.
	Apply func(ctx context.Context, sink events.Sink) error
}

// Options controls a Run.
type Options struct {
	// Skip holds the step IDs given to --skip-step.
	Skip []string
}

// Run runs steps in order. It is a stub until ticket 011.
func Run(ctx context.Context, sink events.Sink, steps []Step, opts Options) error {
	return errors.New("steps.Run: not implemented (ticket 011)")
}
