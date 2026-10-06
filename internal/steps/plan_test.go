package steps_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

func TestPlanReportsWhatRunWouldDo(t *testing.T) {
	w := newWorld("done")
	noCheck := w.step("no-check")
	noCheck.Check = nil
	broken := w.step("broken")
	broken.Check = func(context.Context) (bool, error) {
		w.calls = append(w.calls, "check broken")
		return false, errors.New("database not running")
	}
	list := []steps.Step{noCheck, w.step("done"), w.step("flagged"), broken, w.step("pending")}

	plan, err := steps.Plan(context.Background(), list, steps.Options{Skip: []string{"flagged"}})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, w, "check done", "check broken", "check pending")
	want := []steps.Planned{
		{ID: "no-check", Title: "Title of no-check", Status: steps.PlanApply},
		{ID: "done", Title: "Title of done", Status: steps.PlanDone},
		{ID: "flagged", Title: "Title of flagged", Status: steps.PlanSkipped},
		{ID: "broken", Title: "Title of broken", Status: steps.PlanUnknown, Error: "database not running"},
		{ID: "pending", Title: "Title of pending", Status: steps.PlanApply},
	}
	if !slices.Equal(plan, want) {
		t.Errorf("plan:\n  got  %+v\n  want %+v", plan, want)
	}
}

func TestPlannedJSON(t *testing.T) {
	got, err := json.Marshal([]steps.Planned{
		{ID: "db", Title: "Start the database", Status: steps.PlanDone},
		{ID: "migrate", Title: "Run the migrations", Status: steps.PlanUnknown, Error: "database not running"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":"db","title":"Start the database","status":"done"},` +
		`{"id":"migrate","title":"Run the migrations","status":"unknown","error":"database not running"}]`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestPlanRejectsUnknownSkipStep(t *testing.T) {
	w := newWorld()
	_, err := steps.Plan(context.Background(), []steps.Step{w.step("a")}, steps.Options{Skip: []string{"b"}})
	if code := exitcode.FromError(err); code != exitcode.CodeUsage {
		t.Errorf("exit code %d, want %d (err %v)", code, exitcode.CodeUsage, err)
	}
	assertCalls(t, w)
}

func TestPlanCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newWorld()
	a := w.step("a")
	a.Check = func(ctx context.Context) (bool, error) {
		w.calls = append(w.calls, "check a")
		cancel()
		return false, ctx.Err()
	}

	plan, err := steps.Plan(ctx, []steps.Step{a, w.step("b")}, steps.Options{})
	if plan != nil {
		t.Errorf("plan = %+v, want nil", plan)
	}
	assertCalls(t, w, "check a")
	if se := stepError(t, err); se.Step != "a" || !se.Interrupted || !errors.Is(err, context.Canceled) {
		t.Errorf("error = %+v, want an interruption at a wrapping context.Canceled", se)
	}
}

func TestPlanKeepsTheLastAnswerWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newWorld()
	a := w.step("a")
	a.Check = func(context.Context) (bool, error) {
		cancel()
		return true, nil
	}

	plan, err := steps.Plan(ctx, []steps.Step{a}, steps.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []steps.Planned{{ID: "a", Title: "Title of a", Status: steps.PlanDone}}; !slices.Equal(plan, want) {
		t.Errorf("plan = %+v, want %+v", plan, want)
	}
}
