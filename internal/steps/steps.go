package steps

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
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
	// Skip holds the step IDs given to --skip-step. An ID that names none
	// of the steps is a usage error.
	Skip []string
}

// Run runs steps in order. For each step it emits StepStarted, then:
//   - if opts.Skip names the step, a Warning and StepDone{skipped}, without
//     calling Check or Apply;
//   - if Check reports the step done, StepDone{skipped};
//   - otherwise it calls Apply and emits StepDone{ok} or StepDone{failed}.
//
// Run stops at the first failure and returns an *Error naming the step. A
// re-run resumes there, because Check skips the steps already done.
//
// Once ctx is done, Run starts no more steps and returns an *Error marked
// Interrupted that names the next one. A Check or Apply that is running when ctx ends is left to
// return by itself, so its deferred cleanups run before Run returns. If it
// fails, or Check finds the step not done, the step is reported failed. If
// Apply succeeds or Check finds the step done, the step counts as done, so
// when it was the last one Run returns nil.
//
// Before running anything, Run rejects what Validate rejects.
func Run(ctx context.Context, sink events.Sink, steps []Step, opts Options) error {
	if sink == nil {
		sink = events.Discard
	}
	skip, err := prepare(steps, opts)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			return interrupted(ctx, s.ID)
		}
		sink.Emit(events.StepStarted{ID: s.ID, Title: s.Title})
		status, err := runStep(ctx, sink, s, skip[s.ID])
		sink.Emit(events.StepDone{ID: s.ID, Status: status})
		if err != nil {
			return err
		}
	}
	return nil
}

// runStep runs one step after its StepStarted and returns the status for
// its StepDone.
func runStep(ctx context.Context, sink events.Sink, s Step, skipped bool) (events.StepStatus, error) {
	if skipped {
		sink.Emit(events.Warning{ID: s.ID, Text: "skipped by --skip-step"})
		return events.StepSkipped, nil
	}
	if s.Check != nil {
		done, err := s.Check(ctx)
		if err != nil {
			return events.StepFailed, failed(ctx, s.ID, fmt.Errorf("check: %w", err))
		}
		if done {
			return events.StepSkipped, nil
		}
	}
	if ctx.Err() != nil {
		return events.StepFailed, interrupted(ctx, s.ID)
	}
	if err := s.Apply(ctx, sink); err != nil {
		return events.StepFailed, failed(ctx, s.ID, err)
	}
	return events.StepOK, nil
}

// Error is the error Run returns when a step fails or the run is
// interrupted. The cli layer finds it with errors.As to fill in
// events.ErrorInfo.Step.
type Error struct {
	// Step is the ID of the step a re-run resumes from: the one that failed
	// or was interrupted, or the next one when the run was interrupted
	// between steps.
	Step string
	// Interrupted reports that the run's context ended. Err is then
	// context.Cause(ctx) rather than whatever the step returned as it was
	// cut short, so the exit code comes from the cause: 130 for a
	// cancellation, 128+N for a signal.
	Interrupted bool
	// Err is what Check or Apply returned, or the context's cause.
	Err error
}

func (e *Error) Error() string {
	if e.Interrupted {
		return fmt.Sprintf("stopped at step %s: %v; re-run the command to resume from it", e.Step, e.Err)
	}
	return fmt.Sprintf("step %s failed: %v; re-run the command to retry it (steps already done are skipped)", e.Step, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// failed is the *Error for step id, which returned err. If ctx has ended,
// err is most likely the step noticing that, so the run counts as
// interrupted instead.
func failed(ctx context.Context, id string, err error) error {
	if ctx.Err() != nil {
		return interrupted(ctx, id)
	}
	return &Error{Step: id, Err: err}
}

func interrupted(ctx context.Context, id string) error {
	return &Error{Step: id, Interrupted: true, Err: context.Cause(ctx)}
}

var kebabCase = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Validate reports what Run would reject before running anything:
// a malformed step list (an empty, duplicate or non-kebab-case ID, or a nil
// Apply), which is a bug, and a Skip ID that names none of the steps, which
// is an exitcode.Usage error. An operation that does work before calling
// Run, such as prompting or taking a lock, calls it first.
func Validate(steps []Step, opts Options) error {
	_, err := prepare(steps, opts)
	return err
}

// prepare validates steps and opts, and returns the set of step IDs to
// skip.
func prepare(steps []Step, opts Options) (map[string]bool, error) {
	ids := make([]string, 0, len(steps))
	known := make(map[string]bool, len(steps))
	for i, s := range steps {
		switch {
		case !kebabCase.MatchString(s.ID):
			return nil, fmt.Errorf("steps: step %d has ID %q, which is not kebab-case", i, s.ID)
		case known[s.ID]:
			return nil, fmt.Errorf("steps: duplicate step ID %q", s.ID)
		case s.Apply == nil:
			return nil, fmt.Errorf("steps: step %q has no Apply", s.ID)
		}
		known[s.ID] = true
		ids = append(ids, s.ID)
	}

	skip := make(map[string]bool, len(opts.Skip))
	var unknown []string
	for _, id := range opts.Skip {
		if !known[id] {
			unknown = append(unknown, fmt.Sprintf("%q", id))
			continue
		}
		skip[id] = true
	}
	if len(unknown) > 0 {
		have := "this command has no steps"
		if len(ids) > 0 {
			have = "this command's steps are: " + strings.Join(ids, ", ")
		}
		return nil, exitcode.Usage("--skip-step: no step %s; %s", strings.Join(unknown, ", "), have)
	}
	return skip, nil
}
