package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Teardown step IDs (§9.8).
const (
	StepTeardownDown = "down"
	StepVolumes      = "volumes"
	StepDevImages    = "dev-images"
	StepFiles        = "files"
)

// TeardownOptions configures Reset and Destroy.
type TeardownOptions struct {
	// Name is the stack's name: its compose project and the value of its
	// stack label.
	Name string
	// KeepDB keeps the database volume (reset --keep-db).
	KeepDB bool
	// PruneImages has destroy also remove the commit-tagged images nothing
	// else uses, by cache prune's rules; Cache must be set.
	PruneImages bool
	Cache       *cache.Cache
}

// TeardownReport is reset's and destroy's report.
type TeardownReport struct {
	Stack string `json:"stack"`
	// Volumes lists the volumes removed.
	Volumes []string `json:"volumes"`
	// KeptVolumes lists the stack's volumes reset kept.
	KeptVolumes []string `json:"kept_volumes,omitempty"`
	// Images lists destroy's dev images removed.
	Images []string `json:"images,omitempty"`
	// Files is what destroy did in the stack directory.
	Files *stack.RemoveReport `json:"files,omitempty"`
	// Pruned is what --prune-images removed.
	Pruned *PruneReport `json:"pruned,omitempty"`
}

// Reset removes the stack's containers and its data, TLS and (unless
// KeepDB) database volumes, keeping its config, secrets, logs and TLS
// sources, so the next up re-converges (§9.8). It forgets what state.json
// recorded about the volumes' contents, so up copies them again.
func Reset(ctx context.Context, d *Deps, st *stack.Stack, opts TeardownOptions) (*TeardownReport, error) {
	report := &TeardownReport{Stack: opts.Name, Volumes: []string{}}
	state, err := st.LoadState()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		state = nil
	case err != nil:
		return nil, err
	default:
		state.StartOperation("reset", d.Clock.Now())
		if err := st.SaveState(state); err != nil {
			return nil, err
		}
	}
	plan := []steps.Step{
		downStep(d),
		volumesStep(d, st, opts.Name, report, func(v catalog.Volume) bool {
			switch v.Kind {
			case catalog.KindData, catalog.KindTLS:
				return true
			case catalog.KindDatabase:
				return !opts.KeepDB
			}
			return false
		}),
	}
	runErr := steps.Run(ctx, d.Sink, plan, steps.Options{})
	if state == nil {
		return report, runErr
	}
	// Forget the copies even after a failure: some volumes may be gone.
	state.TLS, state.Truststore, state.HPDSKey = nil, nil, nil
	state.FinishOperation(runErr, d.Clock.Now())
	if err := st.SaveState(state); err != nil && runErr == nil {
		runErr = err
	}
	return report, runErr
}

// Destroy removes everything the CLI created for the stack (§9.8): its
// containers, every volume labelled for it, its dev images, and the paths
// manifest.json lists. Shared data sets, operator files and other stacks'
// resources are never touched. With PruneImages it then prunes the
// commit-tagged images nothing uses any more. The caller holds the stack
// lock; st is unusable afterwards except for Close.
func Destroy(ctx context.Context, d *Deps, st *stack.Stack, opts TeardownOptions) (*TeardownReport, error) {
	report := &TeardownReport{Stack: opts.Name, Volumes: []string{}, Images: []string{}}
	plan := []steps.Step{
		downStep(d),
		volumesStep(d, st, opts.Name, report, func(catalog.Volume) bool { return true }),
		{
			ID:    StepDevImages,
			Title: "Remove the stack's dev images",
			Apply: func(ctx context.Context, sink events.Sink) error {
				return removeDevImages(ctx, d, sink, opts.Name, report)
			},
		},
		{
			ID:    StepFiles,
			Title: "Remove the files pic-sure created",
			Apply: func(ctx context.Context, sink events.Sink) error {
				files, err := st.RemoveCreated()
				report.Files = &files
				for _, p := range files.Kept {
					sink.Emit(events.Warning{ID: StepFiles, Text: "kept " + st.Path(p) + ": it holds files pic-sure didn't create, or isn't what pic-sure created"})
				}
				return err
			},
		},
	}
	if opts.PruneImages {
		plan = append(plan, steps.Step{
			ID:    StepPrune,
			Title: "Remove shared images no other stack uses",
			Apply: func(ctx context.Context, sink events.Sink) error {
				pruned, err := pruneCache(ctx, d, opts.Cache, sink, PruneOptions{CommitImagesOnly: true})
				report.Pruned = pruned
				return err
			},
		})
	}
	return report, steps.Run(ctx, d.Sink, plan, steps.Options{})
}

// downStep runs compose down. A stack that was never rendered has no
// compose file, and no containers compose started.
func downStep(d *Deps) steps.Step {
	return steps.Step{
		ID:    StepTeardownDown,
		Title: "Stop and remove the stack's containers",
		Apply: func(ctx context.Context, sink events.Sink) error {
			if d.Compose == nil {
				sink.Emit(events.Progress{ID: StepTeardownDown, Text: "the stack was never rendered, so compose started nothing"})
				return nil
			}
			out := events.NewLogWriter(sink, StepTeardownDown, events.StreamStderr)
			err := d.Compose.Down(ctx, docker.ComposeDownOpts{Out: out})
			_ = out.Close()
			return err
		},
	}
}

// volumesStep removes the volumes labelled for stack name in st's
// directory that remove selects. Selection is by label only, never by name
// (§4): a volume labelled with the name but another directory belongs to
// another stack, and a shared data set or host volume is never the stack's,
// whatever its labels say.
func volumesStep(d *Deps, st *stack.Stack, name string, report *TeardownReport, remove func(catalog.Volume) bool) steps.Step {
	return steps.Step{
		ID:    StepVolumes,
		Title: "Remove the stack's volumes",
		Apply: func(ctx context.Context, sink events.Sink) error {
			vols, err := stackVolumes(ctx, d, st, name)
			if err != nil {
				return err
			}
			warnMoved(ctx, d, sink, st, name)
			var failed []string
			for _, v := range vols {
				cv, known := catalog.LookupVolume(v.Labels[stack.LabelComposeVolume])
				if known && cv.Scope != catalog.StackScoped {
					continue
				}
				if !remove(cv) {
					report.KeptVolumes = append(report.KeptVolumes, v.Name)
					continue
				}
				if err := d.Docker.VolumeRemove(ctx, v.Name); err != nil {
					failed = append(failed, v.Name)
					sink.Emit(events.Warning{ID: StepVolumes, Text: fmt.Sprintf("couldn't remove volume %s: %v", v.Name, err)})
					continue
				}
				report.Volumes = append(report.Volumes, v.Name)
				sink.Emit(events.Progress{ID: StepVolumes, Text: "removed volume " + v.Name})
			}
			if len(failed) > 0 {
				return fmt.Errorf("couldn't remove volumes %s", strings.Join(failed, ", "))
			}
			return nil
		},
	}
}

// stackVolumes returns the volumes labelled for stack name whose stack-dir
// label is st's directory.
func stackVolumes(ctx context.Context, d *Deps, st *stack.Stack, name string) ([]docker.Volume, error) {
	vols, err := d.Docker.VolumeList(ctx, stack.LabelStack+"="+name, stack.LabelStackDir+"="+st.Dir)
	if err != nil {
		return nil, fmt.Errorf("listing the stack's volumes: %w", err)
	}
	return vols, nil
}

// warnMoved warns about volumes labelled for stack name in another
// directory, which teardown leaves alone: another stack's, or this one's
// from before its directory moved.
func warnMoved(ctx context.Context, d *Deps, sink events.Sink, st *stack.Stack, name string) {
	vols, err := d.Docker.VolumeList(ctx, stack.LabelStack+"="+name)
	if err != nil {
		return
	}
	for _, v := range vols {
		if dir := v.Labels[stack.LabelStackDir]; dir != st.Dir {
			sink.Emit(events.Warning{ID: StepVolumes, Text: fmt.Sprintf("left volume %s alone: it is labelled for stack %s in %q, not this directory", v.Name, name, dir)})
		}
	}
}

// removeDevImages removes the dev-<name>-* images of the built catalog
// images.
func removeDevImages(ctx context.Context, d *Deps, sink events.Sink, name string, report *TeardownReport) error {
	imgs, err := d.Docker.ImageList(ctx, catalog.Namespace+"/*")
	if err != nil {
		return fmt.Errorf("listing images: %w", err)
	}
	var failed []string
	for _, img := range imgs {
		if devStack, ok := cachedImage(img.Ref); !ok || devStack != name {
			continue
		}
		if err := d.Docker.RemoveImage(ctx, img.Ref); err != nil {
			failed = append(failed, img.Ref)
			sink.Emit(events.Warning{ID: StepDevImages, Text: fmt.Sprintf("couldn't remove image %s: %v", img.Ref, err)})
			continue
		}
		report.Images = append(report.Images, img.Ref)
		sink.Emit(events.Progress{ID: StepDevImages, Text: "removed image " + img.Ref})
	}
	if len(failed) > 0 {
		return fmt.Errorf("couldn't remove images %s", strings.Join(failed, ", "))
	}
	return nil
}
