package steps

import "context"

// PlanStatus is what Run would do with a step, as far as Check can tell
// now.
type PlanStatus string

// Plan statuses.
const (
	PlanApply   PlanStatus = "apply"   // Check says it isn't done, or there is no Check
	PlanDone    PlanStatus = "done"    // Check says it's done, so Run would skip it
	PlanSkipped PlanStatus = "skipped" // --skip-step names it
	PlanUnknown PlanStatus = "unknown" // Check failed; Error says why
)

// Planned is one step of a plan. Its JSON form is part of reports such as
// `update --dry-run --json`.
type Planned struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Status PlanStatus `json:"status"`
	Error  string     `json:"error,omitempty"` // Check's error, for PlanUnknown
}

// Plan reports, step by step, what Run would do with the same arguments,
// without applying anything. It calls each Check except for the steps
// opts.Skip names, and emits no events: the caller reports the plan.
//
// A failing Check doesn't stop the plan; the step is PlanUnknown. Checks
// see the state as it is now, so a step whose Check needs an earlier step
// to have been applied may come out PlanUnknown or PlanApply.
//
// Plan rejects what Validate rejects. It returns an Interrupted *Error if
// ctx ends before it has the last Check's answer; a Check that fails after
// ctx ended counts as interrupted, not PlanUnknown.
func Plan(ctx context.Context, steps []Step, opts Options) ([]Planned, error) {
	skip, err := prepare(steps, opts)
	if err != nil {
		return nil, err
	}
	plan := make([]Planned, 0, len(steps))
	for _, s := range steps {
		if ctx.Err() != nil {
			return nil, interrupted(ctx, s.ID)
		}
		p := Planned{ID: s.ID, Title: s.Title, Status: PlanApply}
		switch {
		case skip[s.ID]:
			p.Status = PlanSkipped
		case s.Check != nil:
			done, err := s.Check(ctx)
			switch {
			case err != nil && ctx.Err() != nil:
				return nil, interrupted(ctx, s.ID)
			case err != nil:
				p.Status, p.Error = PlanUnknown, err.Error()
			case done:
				p.Status = PlanDone
			}
		}
		plan = append(plan, p)
	}
	return plan, nil
}
