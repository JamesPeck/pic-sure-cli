package ops

import (
	"context"
	"errors"
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
			"with `pic-sure data load-genomic --promote`, publish that under a new name, and set hpds.shared_name to it", what, cfg.HPDS.SharedName)
	}
	return exitcode.Precondition("%s; recover it with `pic-sure data load-genomic --promote` first", what)
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
