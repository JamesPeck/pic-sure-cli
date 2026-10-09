package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// leftoverPattern is a sh case pattern matching what an interrupted promote
// leaves in a genomic store.
const leftoverPattern = promotePrefix + "*|" + oldPrefix + "*"

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

// GenomicLeftovers returns the genomic store's volume (genomicStoreVolume)
// and the .promote-* and .old-* directories an interrupted promote left in
// it, which HPDS would load as partitions. A volume that doesn't exist
// holds none, and isn't created.
func GenomicLeftovers(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config) (string, []string, error) {
	vol := genomicStoreVolume(cfg)
	if _, err := d.Docker.VolumeInspect(ctx, vol); errors.Is(err, docker.ErrNotFound) {
		return vol, nil, nil
	} else if err != nil {
		return vol, nil, err
	}
	l := &loader{d: d, st: st, cfg: cfg}
	script := `cd "$1"; for p in * .[!.]* ..?*; do if [ -d "$p" ]; then case "$p" in ` + leftoverPattern + `) printf '%s\n' "$p";; esac; fi; done`
	var out bytes.Buffer
	mounts := []docker.Mount{{Source: vol, Target: "/data", ReadOnly: true}}
	if err := l.script(ctx, "genomic-leftovers", mounts, script, []string{"/data"}, nil, &out); err != nil {
		return vol, nil, fmt.Errorf("listing volume %s: %w", vol, err)
	}
	return vol, strings.FieldsFunc(out.String(), func(r rune) bool { return r == '\n' }), nil
}

// genomicLeftoversError is exit 3 for leftovers in vol, with the way to
// recover them.
func genomicLeftoversError(cfg *stack.Config, vol string, leftovers []string) error {
	what := fmt.Sprintf("volume %s holds what an interrupted promote left (%s), which HPDS would load as partitions",
		vol, strings.Join(leftovers, ", "))
	if cfg.HPDS.Data == stack.HPDSShared {
		return exitcode.Precondition("%s; shared data set %s can't be changed: recover it in the stack that published it "+
			"with `pic-sure data load-genomic --promote`, publish that under a new name, and set hpds.shared_name to it", what, cfg.HPDS.SharedName)
	}
	return exitcode.Precondition("%s; recover it with `pic-sure data load-genomic --promote` first", what)
}

// refuseGenomicLeftovers makes up's start step refuse (exit 3), before it
// starts anything, while the genomic store holds what an interrupted
// promote left.
func refuseGenomicLeftovers(d *Deps, st *stack.Stack, cfg *stack.Config, start steps.Step) steps.Step {
	apply := start.Apply
	start.Apply = func(ctx context.Context, sink events.Sink) error {
		vol, leftovers, err := GenomicLeftovers(ctx, d, st, cfg)
		if err != nil {
			return fmt.Errorf("checking for what an interrupted genomic promote left: %w", err)
		}
		if len(leftovers) > 0 {
			return genomicLeftoversError(cfg, vol, leftovers)
		}
		return apply(ctx, sink)
	}
	return start
}
