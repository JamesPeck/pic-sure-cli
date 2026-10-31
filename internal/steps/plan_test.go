package steps_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

func TestPlanStopsChecksAtFirstAction(t *testing.T) {
	for _, first := range []string{"check", "always", "blocked"} {
		t.Run(first, func(t *testing.T) {
			w := newWorld("ready", "later")
			list := []steps.Step{w.step("ready"), w.step("change"), w.step("later"), w.step("start"), w.step("skipped")}
			list[3].Check = nil
			opts := steps.PlanOptions{Skip: []string{"skipped"}}
			boundary := steps.WouldAct
			if first == "always" {
				list[1].Check = nil
				boundary = steps.AlwaysRuns
			}
			if first == "blocked" {
				opts.BlockedChecks = map[string]string{"change": "unsafe Check"}
				boundary = steps.DependsOnEarlierStep
			}
			got, err := steps.Plan(context.Background(), list, opts)
			if err != nil {
				t.Fatal(err)
			}
			want := []steps.PlanStatus{steps.WouldSkip, boundary, steps.DependsOnEarlierStep, steps.AlwaysRuns, steps.WouldSkip}
			for i, p := range got {
				if p.Status != want[i] || p.Reason == "" {
					t.Errorf("step %d = %+v, want %s with reason", i, p, want[i])
				}
			}
			calls := []string{"check ready"}
			if first == "check" {
				calls = append(calls, "check change")
			}
			if !slices.Equal(w.calls, calls) {
				t.Fatalf("calls = %v, want %v", w.calls, calls)
			}
		})
	}
}

func TestPlanExplicitSkipsDoNotBlockLaterChecks(t *testing.T) {
	w := newWorld("last")
	list := []steps.Step{w.step("render"), w.step("unsafe"), w.step("last")}
	list[0].Check = nil
	got, err := steps.Plan(context.Background(), list, steps.PlanOptions{Skip: []string{"render", "unsafe"}, BlockedChecks: map[string]string{"unsafe": "unsafe"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p.Status != steps.WouldSkip {
			t.Errorf("%+v", p)
		}
	}
	if !slices.Equal(w.calls, []string{"check last"}) {
		t.Fatalf("calls = %v", w.calls)
	}
}

func TestPlanErrorsAndCancellation(t *testing.T) {
	w := newWorld("a")
	s := w.step("a")
	_, err := steps.Plan(context.Background(), []steps.Step{s}, steps.PlanOptions{Skip: []string{"unknown"}})
	if err == nil || len(w.calls) != 0 {
		t.Fatalf("validation: %v, calls %v", err, w.calls)
	}
	boom := errors.New("broken check")
	s.Check = func(context.Context) (bool, error) { return false, boom }
	_, err = steps.Plan(context.Background(), []steps.Step{s}, steps.PlanOptions{})
	var se *steps.Error
	if !errors.Is(err, boom) || !errors.As(err, &se) || se.Step != "a" {
		t.Fatalf("check error = %v", err)
	}
	for _, during := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		s.Check = func(context.Context) (bool, error) { cancel(); return true, nil }
		if !during {
			cancel()
		}
		_, err = steps.Plan(ctx, []steps.Step{s}, steps.PlanOptions{})
		cancel()
		if !errors.Is(err, context.Canceled) || !errors.As(err, &se) || !se.Interrupted {
			t.Fatalf("cancel = %v", err)
		}
	}
}
