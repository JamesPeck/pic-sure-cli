package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
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
	// Cache, if set, is the cache whose stack registry destroy removes the
	// stack from.
	Cache *cache.Cache
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
	// LeftAlone lists another stack's resources that use this stack's
	// name, which destroy in a copy of that stack leaves alone.
	LeftAlone []ResourceRef `json:"left_alone,omitempty"`
	// Pruned is what --prune-images removed.
	Pruned *PruneReport `json:"pruned,omitempty"`
}

// Reset removes the stack's containers and its data, TLS and (unless
// KeepDB) database volumes, keeping its config, secrets, logs and TLS
// sources, so the next up re-converges (§9.8). It forgets what state.json
// recorded about the volumes' contents, so up copies them again. The
// stack's name selecting another stack's resources (in a copy, the
// original's) is exit 3, before anything changes.
func Reset(ctx context.Context, d *Deps, st *stack.Stack, opts TeardownOptions) (*TeardownReport, error) {
	report := &TeardownReport{Stack: opts.Name, Volumes: []string{}}
	owned, err := CheckOwnership(ctx, d, st, opts.Name)
	if err != nil {
		return nil, err
	}
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
		volumesStep(d, owned, report, func(v catalog.Volume) bool {
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
//
// When the stack's name selects another stack's resources, st is a copy of
// that stack (or shares its name), and compose down, the volumes and the
// dev images would all be the other stack's: destroy removes only st's
// files and lists the rest in LeftAlone.
func Destroy(ctx context.Context, d *Deps, st *stack.Stack, opts TeardownOptions) (*TeardownReport, error) {
	report := &TeardownReport{Stack: opts.Name, Volumes: []string{}, Images: []string{}}
	owned, err := StackResources(ctx, d, opts.Name, st.ID(), st.Dir)
	if err != nil {
		return nil, err
	}
	NoteMoved(d.Sink, owned)
	var plan []steps.Step
	if report.LeftAlone = Refs(owned.Foreign()); len(report.LeftAlone) > 0 {
		d.Sink.Emit(events.Warning{ID: StepFiles, Text: fmt.Sprintf("the stack name %s is in use by another stack's Docker resources, so destroy removes only this directory's files and leaves these alone:\n%s",
			opts.Name, ResourceList(report.LeftAlone))})
		if own := Refs(slices.DeleteFunc(slices.Clone(owned.Resources), func(r Resource) bool { return r.Claim == stack.Foreign })); len(own) > 0 {
			d.Sink.Emit(events.Warning{ID: StepFiles, Text: fmt.Sprintf("this stack's own Docker resources are left too, since compose down would reach the other stack's; remove them with docker once that is sorted out:\n%s",
				ResourceList(own))})
		}
	} else {
		plan = []steps.Step{
			downStep(d),
			volumesStep(d, owned, report, func(catalog.Volume) bool { return true }),
			{
				ID:    StepDevImages,
				Title: "Remove the stack's dev images",
				Apply: func(ctx context.Context, sink events.Sink) error {
					return removeDevImages(ctx, d, sink, st, opts.Name, report)
				},
			},
		}
	}
	plan = append(plan,
		steps.Step{
			ID:    StepFiles,
			Title: "Remove the files pic-sure created",
			Apply: func(ctx context.Context, sink events.Sink) error {
				files, err := st.RemoveCreated()
				report.Files = &files
				for _, p := range files.Kept {
					sink.Emit(events.Warning{ID: StepFiles, Text: "kept " + st.Path(p) + ": it holds files pic-sure didn't create, or isn't what pic-sure created"})
				}
				if err != nil || opts.Cache == nil {
					return err
				}
				// The stack is gone either way; a stale entry is one prune
				// forgets.
				if err := opts.Cache.UnregisterStack(st.Dir); err != nil {
					sink.Emit(events.Warning{ID: StepFiles, Text: "couldn't drop the stack from the cache's registry: " + err.Error()})
				}
				return nil
			},
		})
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

// volumesStep removes the stack's volumes in owned that remove selects.
// Selection is by label only, never by name (§4), and a shared data set or
// host volume is never the stack's, whatever its labels say.
func volumesStep(d *Deps, owned *Ownership, report *TeardownReport, remove func(catalog.Volume) bool) steps.Step {
	return steps.Step{
		ID:    StepVolumes,
		Title: "Remove the stack's volumes",
		Apply: func(ctx context.Context, sink events.Sink) error {
			var failed []string
			for _, v := range owned.Resources {
				if v.Kind != "volume" || v.Claim == stack.Foreign {
					continue
				}
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

// removeDevImages removes the dev-<name>-* images of the built catalog
// images that are st's: labelled for it by the ownership rule, or from
// before dev images had stack labels.
func removeDevImages(ctx context.Context, d *Deps, sink events.Sink, st *stack.Stack, name string, report *TeardownReport) error {
	imgs, err := d.Docker.ImageList(ctx, catalog.Namespace+"/*")
	if err != nil {
		return fmt.Errorf("listing images: %w", err)
	}
	id := st.ID()
	var failed []string
	for _, img := range imgs {
		if devStack, ok := cachedImage(img.Ref); !ok || devStack != name {
			continue
		}
		if _, labelled := img.Labels[stack.LabelStackDir]; labelled && stack.Owner(id, st.Dir, img.Labels) == stack.Foreign {
			sink.Emit(events.Warning{ID: StepDevImages, Text: fmt.Sprintf("left image %s alone: it belongs to %s", img.Ref, stack.OwnerName(img.Labels))})
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
