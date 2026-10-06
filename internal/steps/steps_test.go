package steps_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// world records which Checks and Applies ran, in order, and holds the
// state Checks inspect: a step is done once its ID is in done.
type world struct {
	calls []string
	done  map[string]bool
}

func newWorld(done ...string) *world {
	w := &world{done: map[string]bool{}}
	for _, id := range done {
		w.done[id] = true
	}
	return w
}

// step returns a step whose Check reports w.done[id] and whose Apply marks
// it done.
func (w *world) step(id string) steps.Step {
	return steps.Step{
		ID:    id,
		Title: "Title of " + id,
		Check: func(context.Context) (bool, error) {
			w.calls = append(w.calls, "check "+id)
			return w.done[id], nil
		},
		Apply: func(context.Context, events.Sink) error {
			w.calls = append(w.calls, "apply "+id)
			w.done[id] = true
			return nil
		},
	}
}

// describe renders events compactly for comparison.
func describe(evs []events.Event) []string {
	var out []string
	for _, e := range evs {
		switch e := e.(type) {
		case events.StepStarted:
			out = append(out, "started "+e.ID+" ("+e.Title+")")
		case events.StepDone:
			out = append(out, "done "+e.ID+" "+string(e.Status))
		case events.Warning:
			out = append(out, "warning "+e.ID+": "+e.Text)
		case events.Progress:
			out = append(out, "progress "+e.ID+": "+e.Text)
		default:
			out = append(out, e.Type())
		}
	}
	return out
}

func assertEvents(t *testing.T, rec *events.Recorder, want ...string) {
	t.Helper()
	if got := describe(rec.Events()); !slices.Equal(got, want) {
		t.Errorf("events:\n  got  %q\n  want %q", got, want)
	}
}

func assertCalls(t *testing.T, w *world, want ...string) {
	t.Helper()
	if !slices.Equal(w.calls, want) {
		t.Errorf("calls:\n  got  %q\n  want %q", w.calls, want)
	}
}

// stepError returns err's *steps.Error, failing the test if there is none.
func stepError(t *testing.T, err error) *steps.Error {
	t.Helper()
	var se *steps.Error
	if !errors.As(err, &se) {
		t.Fatalf("error %v (%T) is not a *steps.Error", err, err)
	}
	return se
}

func TestRunAppliesStepsInOrder(t *testing.T) {
	w := newWorld()
	a := w.step("a")
	a.Check = nil // always applies
	b := w.step("b")
	apply := b.Apply
	b.Apply = func(ctx context.Context, sink events.Sink) error {
		sink.Emit(events.Progress{ID: "b", Text: "halfway"})
		return apply(ctx, sink)
	}
	var rec events.Recorder

	if err := steps.Run(context.Background(), &rec, []steps.Step{a, b, w.step("c")}, steps.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, w, "apply a", "check b", "apply b", "check c", "apply c")
	assertEvents(t, &rec,
		"started a (Title of a)", "done a ok",
		"started b (Title of b)", "progress b: halfway", "done b ok",
		"started c (Title of c)", "done c ok",
	)
}

func TestRunSkipsStepsCheckSaysAreDone(t *testing.T) {
	w := newWorld("a", "c")
	var rec events.Recorder

	err := steps.Run(context.Background(), &rec, []steps.Step{w.step("a"), w.step("b"), w.step("c")}, steps.Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, w, "check a", "check b", "apply b", "check c")
	assertEvents(t, &rec,
		"started a (Title of a)", "done a skipped",
		"started b (Title of b)", "done b ok",
		"started c (Title of c)", "done c skipped",
	)
}

func TestRunSkipsStepsNamedBySkipStep(t *testing.T) {
	w := newWorld()
	b := w.step("b")
	b.Check = nil
	var rec events.Recorder

	err := steps.Run(context.Background(), &rec, []steps.Step{w.step("a"), b, w.step("c")},
		steps.Options{Skip: []string{"c", "b", "c"}})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, w, "check a", "apply a")
	assertEvents(t, &rec,
		"started a (Title of a)", "done a ok",
		"started b (Title of b)", "warning b: skipped by --skip-step", "done b skipped",
		"started c (Title of c)", "warning c: skipped by --skip-step", "done c skipped",
	)
}

func TestRunRejectsUnknownSkipStep(t *testing.T) {
	tests := []struct {
		name  string
		steps []string
		skip  []string
		want  string
	}{
		{
			name:  "unknown IDs",
			steps: []string{"db", "db-migrate"},
			skip:  []string{"db", "migrate", "seed"},
			want:  `--skip-step: no step "migrate", "seed"; this command's steps are: db, db-migrate`,
		},
		{
			name: "no steps",
			skip: []string{"db"},
			want: `--skip-step: no step "db"; this command has no steps`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld()
			var list []steps.Step
			for _, id := range tt.steps {
				list = append(list, w.step(id))
			}
			var rec events.Recorder

			validateErr := steps.Validate(list, steps.Options{Skip: tt.skip})
			err := steps.Run(context.Background(), &rec, list, steps.Options{Skip: tt.skip})
			for _, err := range []error{validateErr, err} {
				if code := exitcode.FromError(err); code != exitcode.CodeUsage {
					t.Errorf("exit code %d, want %d (err %v)", code, exitcode.CodeUsage, err)
				}
				if err == nil || err.Error() != tt.want {
					t.Errorf("error %v, want %q", err, tt.want)
				}
			}
			assertCalls(t, w)
			assertEvents(t, &rec)
		})
	}
}

func TestRunStopsAtTheFirstFailure(t *testing.T) {
	w := newWorld()
	boom := errors.New("flyway exited 1")
	b := w.step("b")
	b.Apply = func(context.Context, events.Sink) error {
		w.calls = append(w.calls, "apply b")
		return boom
	}
	var rec events.Recorder

	err := steps.Run(context.Background(), &rec, []steps.Step{w.step("a"), b, w.step("c")}, steps.Options{})
	assertCalls(t, w, "check a", "apply a", "check b", "apply b")
	assertEvents(t, &rec,
		"started a (Title of a)", "done a ok",
		"started b (Title of b)", "done b failed",
	)
	se := stepError(t, err)
	if se.Step != "b" || se.Interrupted || !errors.Is(err, boom) {
		t.Errorf("error = %+v, want step b, not interrupted, wrapping %v", se, boom)
	}
	want := "step b failed: flyway exited 1; re-run the command to retry it (steps already done are skipped)"
	if err.Error() != want {
		t.Errorf("message:\n  got  %q\n  want %q", err, want)
	}
	if code := exitcode.FromError(err); code != exitcode.CodeFailed {
		t.Errorf("exit code %d, want %d", code, exitcode.CodeFailed)
	}
}

func TestRunFailsAStepWhoseCheckFails(t *testing.T) {
	w := newWorld()
	unreachable := errors.New("docker daemon unreachable")
	b := w.step("b")
	b.Check = func(context.Context) (bool, error) {
		w.calls = append(w.calls, "check b")
		return false, unreachable
	}
	var rec events.Recorder

	err := steps.Run(context.Background(), &rec, []steps.Step{b, w.step("c")}, steps.Options{})
	assertCalls(t, w, "check b")
	assertEvents(t, &rec, "started b (Title of b)", "done b failed")
	if se := stepError(t, err); se.Step != "b" || !errors.Is(err, unreachable) {
		t.Errorf("error = %+v, want step b wrapping %v", se, unreachable)
	}
	if !strings.Contains(err.Error(), "check: docker daemon unreachable") {
		t.Errorf("message %q doesn't say the check failed", err)
	}
}

func TestRunKeepsTheStepsExitCode(t *testing.T) {
	w := newWorld()
	a := w.step("a")
	a.Apply = func(context.Context, events.Sink) error {
		return exitcode.Precondition("ports busy: %d", 443)
	}

	err := steps.Run(context.Background(), events.Discard, []steps.Step{a}, steps.Options{})
	if code := exitcode.FromError(err); code != exitcode.CodePrecondition {
		t.Errorf("exit code %d, want %d (err %v)", code, exitcode.CodePrecondition, err)
	}
}

func TestRunResumesWhereAFailedRunStopped(t *testing.T) {
	w := newWorld()
	broken := true
	b := w.step("b")
	apply := b.Apply
	b.Apply = func(ctx context.Context, sink events.Sink) error {
		if broken {
			w.calls = append(w.calls, "apply b")
			return errors.New("boom")
		}
		return apply(ctx, sink)
	}
	list := []steps.Step{w.step("a"), b, w.step("c")}

	if err := steps.Run(context.Background(), events.Discard, list, steps.Options{}); err == nil {
		t.Fatal("first run succeeded, want a failure at b")
	}
	broken = false
	w.calls = nil
	var rec events.Recorder
	if err := steps.Run(context.Background(), &rec, list, steps.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, w, "check a", "check b", "apply b", "check c", "apply c")
	assertEvents(t, &rec,
		"started a (Title of a)", "done a skipped",
		"started b (Title of b)", "done b ok",
		"started c (Title of c)", "done c ok",
	)
}

func TestRunCancelledInsideApply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newWorld()
	var cleanedUp bool
	var cleanupCtxErr error
	b := w.step("b")
	b.Apply = func(ctx context.Context, _ events.Sink) error {
		w.calls = append(w.calls, "apply b")
		defer func() {
			// The pattern for a cleanup that has to run commands after
			// the run's context has ended.
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer ccancel()
			cleanupCtxErr = cctx.Err()
			cleanedUp = true
		}()
		cancel() // Ctrl-C arrives while the step works
		<-ctx.Done()
		return exitcode.Failed("docker compose up: %w", ctx.Err())
	}
	var rec events.Recorder

	err := steps.Run(ctx, &rec, []steps.Step{w.step("a"), b, w.step("c")}, steps.Options{})
	if !cleanedUp || cleanupCtxErr != nil {
		t.Errorf("cleanup ran: %v, with context error %v; want true, nil", cleanedUp, cleanupCtxErr)
	}
	assertCalls(t, w, "check a", "apply a", "check b", "apply b")
	assertEvents(t, &rec,
		"started a (Title of a)", "done a ok",
		"started b (Title of b)", "done b failed",
	)
	se := stepError(t, err)
	if se.Step != "b" || !se.Interrupted || !errors.Is(err, context.Canceled) {
		t.Errorf("error = %+v, want step b, interrupted, wrapping context.Canceled", se)
	}
	if want := "step b: context canceled; re-run the command to resume from it"; err.Error() != want {
		t.Errorf("message:\n  got  %q\n  want %q", err, want)
	}
	// The step's own exit code (1) gives way to the interruption's.
	if code := exitcode.FromError(err); code != exitcode.CodeInterrupted {
		t.Errorf("exit code %d, want %d", code, exitcode.CodeInterrupted)
	}
}

func TestRunCarriesTheSignalCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	a := steps.Step{ID: "a", Apply: func(ctx context.Context, _ events.Sink) error {
		cancel(exitcode.Signaled(syscall.SIGTERM))
		return ctx.Err()
	}}

	err := steps.Run(ctx, events.Discard, []steps.Step{a}, steps.Options{})
	if code := exitcode.FromError(err); code != 128+int(syscall.SIGTERM) {
		t.Errorf("exit code %d, want %d (err %v)", code, 128+int(syscall.SIGTERM), err)
	}
}

func TestRunCancelledBetweenSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newWorld()
	a := w.step("a")
	apply := a.Apply
	a.Apply = func(ctx context.Context, sink events.Sink) error {
		cancel() // arrives just as a finishes
		return apply(ctx, sink)
	}
	var rec events.Recorder

	err := steps.Run(ctx, &rec, []steps.Step{a, w.step("b")}, steps.Options{})
	assertCalls(t, w, "check a", "apply a")
	assertEvents(t, &rec, "started a (Title of a)", "done a ok")
	if se := stepError(t, err); se.Step != "b" || !se.Interrupted {
		t.Errorf("error = %+v, want an interruption naming b, the next step", se)
	}
}

func TestRunCancelledAsTheLastStepFinishes(t *testing.T) {
	tests := []struct {
		name   string
		step   func(w *world, cancel context.CancelFunc) steps.Step
		calls  []string
		status string
	}{
		{
			name: "apply succeeds",
			step: func(w *world, cancel context.CancelFunc) steps.Step {
				s := w.step("a")
				apply := s.Apply
				s.Apply = func(ctx context.Context, sink events.Sink) error {
					cancel()
					return apply(ctx, sink)
				}
				return s
			},
			calls:  []string{"check a", "apply a"},
			status: "ok",
		},
		{
			name: "check finds it done",
			step: func(w *world, cancel context.CancelFunc) steps.Step {
				s := w.step("a")
				s.Check = func(context.Context) (bool, error) {
					w.calls = append(w.calls, "check a")
					cancel()
					return true, nil
				}
				return s
			},
			calls:  []string{"check a"},
			status: "skipped",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := newWorld()
			var rec events.Recorder

			// Every step is done, so the run succeeded.
			if err := steps.Run(ctx, &rec, []steps.Step{tt.step(w, cancel)}, steps.Options{}); err != nil {
				t.Errorf("Run = %v, want nil", err)
			}
			assertCalls(t, w, tt.calls...)
			assertEvents(t, &rec, "started a (Title of a)", "done a "+tt.status)
		})
	}
}

func TestRunCancelledDuringCheck(t *testing.T) {
	tests := []struct {
		name     string
		checkErr bool // Check notices the cancellation and fails
	}{
		{"check fails", true},
		{"check returns not done", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := newWorld()
			a := w.step("a")
			a.Check = func(ctx context.Context) (bool, error) {
				w.calls = append(w.calls, "check a")
				cancel()
				if tt.checkErr {
					return false, ctx.Err()
				}
				return false, nil
			}
			var rec events.Recorder

			err := steps.Run(ctx, &rec, []steps.Step{a}, steps.Options{})
			assertCalls(t, w, "check a")
			assertEvents(t, &rec, "started a (Title of a)", "done a failed")
			if se := stepError(t, err); se.Step != "a" || !se.Interrupted {
				t.Errorf("error = %+v, want an interruption at a", se)
			}
		})
	}
}

func TestRunWithAContextAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWorld()
	var rec events.Recorder

	err := steps.Run(ctx, &rec, []steps.Step{w.step("a")}, steps.Options{})
	assertCalls(t, w)
	assertEvents(t, &rec)
	if se := stepError(t, err); se.Step != "a" || !se.Interrupted {
		t.Errorf("error = %+v, want an interruption before a", se)
	}
}

func TestRunRejectsMalformedSteps(t *testing.T) {
	apply := func(context.Context, events.Sink) error { return nil }
	tests := []struct {
		name  string
		steps []steps.Step
		want  string
	}{
		{"empty ID", []steps.Step{{Apply: apply}}, `step 0 has ID "", which is not kebab-case`},
		{"not kebab-case", []steps.Step{{ID: "a", Apply: apply}, {ID: "DB_Migrate", Apply: apply}}, `step 1 has ID "DB_Migrate"`},
		{"trailing hyphen", []steps.Step{{ID: "db-", Apply: apply}}, `step 0 has ID "db-"`},
		{"duplicate ID", []steps.Step{{ID: "db", Apply: apply}, {ID: "db", Apply: apply}}, `duplicate step ID "db"`},
		{"nil Apply", []steps.Step{{ID: "db"}}, `step "db" has no Apply`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec events.Recorder
			validateErr := steps.Validate(tt.steps, steps.Options{})
			err := steps.Run(context.Background(), &rec, tt.steps, steps.Options{})
			for _, err := range []error{validateErr, err} {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error %v, want one containing %q", err, tt.want)
				}
				if code := exitcode.FromError(err); code != exitcode.CodeFailed {
					t.Errorf("exit code %d, want %d: a malformed list is a bug, not a usage error", code, exitcode.CodeFailed)
				}
			}
			assertEvents(t, &rec)
		})
	}
}

func TestRunAcceptsKebabCaseIDs(t *testing.T) {
	apply := func(context.Context, events.Sink) error { return nil }
	var list []steps.Step
	for _, id := range []string{"db", "db-migrate", "flyway-init-2", "1000genomes"} {
		list = append(list, steps.Step{ID: id, Apply: apply})
	}
	if err := steps.Validate(list, steps.Options{Skip: []string{"db-migrate"}}); err != nil {
		t.Fatal(err)
	}
	if err := steps.Run(context.Background(), events.Discard, list, steps.Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestRunWithANilSink(t *testing.T) {
	w := newWorld()
	if err := steps.Run(context.Background(), nil, []steps.Step{w.step("a")}, steps.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, w, "check a", "apply a")
}
