package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// GenomicLeftoversStepID is the ID of up's first step, which refuses while
// the genomic store holds what an interrupted promote left.
const GenomicLeftoversStepID = "genomic-leftovers"

// genomicStoreVolume is the Docker name of the genomic store HPDS loads its
// partitions from: hpds-genomic, or with hpds.data: shared the set's
// genomic volume, which each stack's hpds-genomic-copy is seeded from.
func genomicStoreVolume(cfg *stack.Config) string {
	if cfg.HPDS.Data == stack.HPDSShared {
		v, _ := catalog.LookupVolume("shared-hpds-genomic")
		return v.DockerName(cfg.HPDS.SharedName)
	}
	v, _ := catalog.LookupVolume(hpdsGenomicVolume)
	return v.DockerName(cfg.Name)
}

// GenomicLeftovers returns the .promote-* and .old-* directories an
// interrupted promote left in the genomic store (genomicStoreVolume), which
// HPDS would load as partitions. A volume that doesn't exist holds none,
// and isn't created.
func GenomicLeftovers(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config) ([]string, error) {
	vol := genomicStoreVolume(cfg)
	if _, err := d.Docker.VolumeInspect(ctx, vol); errors.Is(err, docker.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	l := &loader{d: d, st: st, cfg: cfg, rmTimeout: dockerProbeTimeout}
	dirs, err := l.dirs(ctx, vol, "/data")
	if err != nil {
		return nil, err
	}
	var leftovers []string
	for _, dir := range dirs {
		if strings.HasPrefix(dir, promotePrefix) || strings.HasPrefix(dir, oldPrefix) {
			leftovers = append(leftovers, dir)
		}
	}
	return leftovers, nil
}

// genomicLeftoversError is exit 3 for leftovers in the genomic store, with
// the way to recover them.
func genomicLeftoversError(cfg *stack.Config, leftovers []string) error {
	what := "volume " + genomicStoreVolume(cfg) + " holds what an interrupted promote left (" + strings.Join(leftovers, ", ") +
		"), which HPDS would load as partitions"
	if cfg.HPDS.Data == stack.HPDSShared {
		return exitcode.Precondition("%s; shared data set %s can't be changed: recover it in the stack that published it "+
			"with `pic-sure data load-genomic --recover`, publish that under a new name, and set hpds.shared_name to it", what, cfg.HPDS.SharedName)
	}
	return exitcode.Precondition("%s; recover it first with `pic-sure data load-genomic --recover` "+
		"(or a `--promote` load, if you also want new data)", what)
}

// genomicLeftoversStep refuses (exit 3) while the genomic store holds
// leftovers: its Check lists them, and its Apply refuses.
func genomicLeftoversStep(d *Deps, st *stack.Stack, cfg *stack.Config) steps.Step {
	var leftovers []string
	return steps.Step{
		ID:    GenomicLeftoversStepID,
		Title: "Check the genomic store for an interrupted promote",
		Check: func(ctx context.Context) (bool, error) {
			var err error
			leftovers, err = GenomicLeftovers(ctx, d, st, cfg)
			return len(leftovers) == 0, err
		},
		Apply: func(context.Context, events.Sink) error { return genomicLeftoversError(cfg, leftovers) },
	}
}

// GenomicRecoverStepID is the ID of `data load-genomic --recover`'s step
// that settles the leftovers, between LoaderStopStepID and
// LoaderStartStepID.
const GenomicRecoverStepID = "genomic-recover"

// GenomicRecovery is what RecoverGenomic found and did.
type GenomicRecovery struct {
	// Leftovers are the .promote- and .old- directories it found.
	Leftovers []string
	// Partitions says what became of each partition they were left for.
	Partitions []RecoveredPartition
	// WasRunning reports that HPDS was running (or restarting) before the
	// recovery, so it is started again.
	WasRunning bool
}

// RecoveredPartition is one partition RecoverGenomic settled. Result is
// "completed" (the promoted copy is live), "restored" (the partition from
// before the promote is live again) or "discarded" (a partial copy was
// removed, and the partition is as it was, or still absent).
type RecoveredPartition struct {
	Partition string
	Result    string
}

// RefuseSharedGenomicRecover is exit 3 for a stack using a shared data set,
// which it can't change, with what that set holds and how to recover it.
func RefuseSharedGenomicRecover(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config) error {
	if cfg.HPDS.Data != stack.HPDSShared {
		return nil
	}
	leftovers, err := GenomicLeftovers(ctx, d, st, cfg)
	if err != nil {
		return err
	}
	if len(leftovers) > 0 {
		return genomicLeftoversError(cfg, leftovers)
	}
	return exitcode.Precondition("this stack's HPDS uses the shared data set %q (hpds.data: shared), which it can't change; "+
		"volume %s holds nothing an interrupted promote left", cfg.HPDS.SharedName, genomicStoreVolume(cfg))
}

// RecoverGenomic is `data load-genomic --recover`: it settles leftovers,
// what GenomicLeftovers found an interrupted promote left in hpds-genomic,
// as a Promote load does before its own promote, without loading anything.
// The caller holds the stack lock and sets d.Compose. With no leftovers it
// changes nothing. Otherwise it stops hpds like a Promote load, settles
// every partition (settleLive), and starts hpds again if it was running; a
// stopped one is left for `pic-sure up`. A shared data set is
// RefuseSharedGenomicRecover's exit 3.
func RecoverGenomic(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, leftovers []string) (GenomicRecovery, error) {
	r := GenomicRecovery{Leftovers: leftovers}
	if cfg.HPDS.Data == stack.HPDSShared {
		return r, RefuseSharedGenomicRecover(ctx, d, st, cfg)
	}
	if len(leftovers) == 0 {
		return r, nil
	}
	g := &genomicLoad{loader: &loader{d: d, st: st, cfg: cfg}, leftovers: leftovers}
	live := g.vol(hpdsGenomicVolume)
	plan := []steps.Step{
		{ID: LoaderStopStepID, Title: "Stop HPDS", Apply: func(ctx context.Context, sink events.Sink) error {
			svc, err := composeService(ctx, d, hpdsService)
			if err != nil {
				return err
			}
			r.WasRunning = svc != nil && (svc.State == "running" || svc.State == "restarting")
			return g.stop(ctx, sink)
		}},
		{ID: GenomicRecoverStepID, Title: "Recover " + live, Apply: func(ctx context.Context, sink events.Sink) error {
			sink.Emit(events.Progress{ID: GenomicRecoverStepID, Text: "recovering what an interrupted promote left in " + live +
				" (" + strings.Join(leftovers, ", ") + ")"})
			out, err := g.settleLive(ctx)
			if err != nil {
				return err
			}
			r.Partitions = recoveredPartitions(leftovers, out)
			for _, p := range r.Partitions {
				sink.Emit(events.Progress{ID: GenomicRecoverStepID, Text: p.Result + " " + p.Partition})
			}
			return nil
		}},
		{ID: LoaderStartStepID, Title: "Start HPDS", Apply: g.start,
			Check: func(context.Context) (bool, error) { return !r.WasRunning, nil }},
	}
	err := steps.Run(ctx, d.Sink, plan, steps.Options{})
	var se *steps.Error
	if !errors.As(err, &se) {
		return r, err
	}
	// steps.Error's advice, that a re-run resumes, doesn't fit: a re-run
	// starts over, which is what settling wants.
	failed := fmt.Errorf("step %s failed: %w", se.Step, se.Err)
	if se.Interrupted {
		failed = fmt.Errorf("stopped at step %s: %w", se.Step, se.Err)
	}
	if se.Step == LoaderStartStepID {
		return r, fmt.Errorf("%w. Volume %s is recovered; see `pic-sure logs hpds`, then start HPDS with `pic-sure up`", failed, live)
	}
	return r, fmt.Errorf("%w. HPDS may be stopped; run `pic-sure data load-genomic --recover` again before starting it", failed)
}

// recoveredPartitions is what settleLive's output says became of each
// partition leftovers were left for. Settle prints nothing when it only
// removes a partial copy.
func recoveredPartitions(leftovers []string, out string) []RecoveredPartition {
	said := map[string]string{}
	for line := range strings.Lines(out) {
		if result, p, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " "); ok {
			said[p] = result
		}
	}
	var ps []RecoveredPartition
	for _, d := range leftovers {
		p := strings.TrimPrefix(strings.TrimPrefix(d, promotePrefix), oldPrefix)
		if slices.ContainsFunc(ps, func(r RecoveredPartition) bool { return r.Partition == p }) {
			continue
		}
		result := said[p]
		if result == "" {
			result = "discarded"
		}
		ps = append(ps, RecoveredPartition{Partition: p, Result: result})
	}
	return ps
}
