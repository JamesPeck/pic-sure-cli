package steps

import (
	"context"
	"fmt"
)

type PlanStatus string

const (
	WouldSkip            PlanStatus = "would_skip"
	WouldAct             PlanStatus = "would_act"
	AlwaysRuns           PlanStatus = "always_runs"
	DependsOnEarlierStep PlanStatus = "depends_on_earlier_step"
)

type PlannedStep struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Status PlanStatus `json:"status"`
	Reason string     `json:"reason"`
}

type PlanOptions struct {
	Skip []string
	// BlockedChecks names Checks that cannot safely inspect the preview's
	// inputs, with the reason. They stop evaluation even when earlier steps
	// were explicitly skipped.
	BlockedChecks map[string]string
}

// Plan never calls Apply. After the first required action, later Checks
// cannot predict the state that action would leave behind.
func Plan(ctx context.Context, list []Step, opts PlanOptions) ([]PlannedStep, error) {
	skip, err := prepare(list, Options{Skip: opts.Skip})
	if err != nil {
		return nil, err
	}
	plan := make([]PlannedStep, 0, len(list))
	boundary := ""
	for _, s := range list {
		if ctx.Err() != nil {
			return plan, interrupted(ctx, s.ID)
		}
		p := PlannedStep{ID: s.ID, Title: s.Title}
		switch {
		case skip[s.ID]:
			p.Status, p.Reason = WouldSkip, "skipped by --skip-step"
		case s.Check == nil:
			p.Status, p.Reason = AlwaysRuns, "no Check; always runs unless explicitly skipped"
			if boundary == "" {
				boundary = s.ID
			}
		case boundary != "":
			p.Status, p.Reason = DependsOnEarlierStep, "not checked: depends on "+boundary
		case opts.BlockedChecks[s.ID] != "":
			p.Status, p.Reason = DependsOnEarlierStep, opts.BlockedChecks[s.ID]
			boundary = s.ID
		default:
			done, err := s.Check(ctx)
			if err != nil {
				return plan, failed(ctx, s.ID, fmt.Errorf("check: %w", err))
			}
			if ctx.Err() != nil {
				return plan, interrupted(ctx, s.ID)
			}
			p.Status, p.Reason = WouldSkip, "Check reports already done"
			if !done {
				p.Status, p.Reason = WouldAct, "Check reports work required"
				boundary = s.ID
			}
		}
		plan = append(plan, p)
	}
	return plan, nil
}
