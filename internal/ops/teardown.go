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
	// DryRun selects resources without changing Docker, files or state.
	DryRun bool
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
	// DryRun and Containers are preview-only additions; real reports retain their wire format.
	DryRun     bool     `json:"dry_run,omitempty"`
	Containers []string `json:"containers,omitempty"`
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
	// LeftOwn lists the stack's own resources destroy in a copy leaves
	// with them, since compose down would reach the other stack's.
	LeftOwn []ResourceRef `json:"left_own,omitempty"`
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
	report := &TeardownReport{Stack: opts.Name, DryRun: opts.DryRun, Volumes: []string{}}
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
	case !opts.DryRun:
		state.StartOperation("reset", d.Clock.Now())
		if err := st.SaveState(state); err != nil {
			return nil, err
		}
	}
	plan := []steps.Step{
		downStep(d, st, opts.Name, owned, report),
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
	if state == nil || opts.DryRun {
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
// lock; st is unusable afterwards except for Close. DryRun only selects
// resources and simulates file removals, leaving the stack usable.
//
// When the stack's name selects another stack's resources, st is a copy of
// that stack (or shares its name), and compose down, the volumes and the
// dev images would all be the other stack's: destroy removes only st's
// files and lists the rest in LeftAlone.
func Destroy(ctx context.Context, d *Deps, st *stack.Stack, opts TeardownOptions) (*TeardownReport, error) {
	report := &TeardownReport{Stack: opts.Name, DryRun: opts.DryRun, Volumes: []string{}, Images: []string{}}
	owned, err := StackResources(ctx, d, opts.Name, st.ID(), st.Dir)
	if err != nil {
		return nil, err
	}
	NoteMoved(d.Sink, owned)
	var plan []steps.Step
	if report.LeftAlone = Refs(owned.Foreign()); len(report.LeftAlone) > 0 {
		d.Sink.Emit(events.Warning{ID: StepFiles, Text: fmt.Sprintf("the stack name %s is in use by another stack's Docker resources, so destroy removes only this directory's files and leaves these alone:\n%s",
			opts.Name, ResourceList(report.LeftAlone))})
		report.LeftOwn = Refs(slices.DeleteFunc(slices.Clone(owned.Resources), func(r Resource) bool { return r.Claim == stack.Foreign }))
		if len(report.LeftOwn) > 0 {
			d.Sink.Emit(events.Warning{ID: StepFiles, Text: fmt.Sprintf("this stack's own Docker resources are left too, since compose down would reach the other stack's; remove them with docker once that is sorted out:\n%s",
				ResourceList(report.LeftOwn))})
		}
	} else {
		plan = []steps.Step{
			downStep(d, st, opts.Name, owned, report),
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
				files, err := st.RemoveCreated(opts.DryRun)
				report.Files = &files
				for _, p := range files.Kept {
					sink.Emit(events.Warning{ID: StepFiles, Text: "kept " + st.Path(p) + ": it holds files pic-sure didn't create, or isn't what pic-sure created"})
				}
				if err != nil || opts.Cache == nil || opts.DryRun {
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
				pruneOpts := PruneOptions{CommitImagesOnly: true, DryRun: opts.DryRun}
				if opts.DryRun {
					projection := &teardownProjection{dir: st.Dir, report: report}
					if len(report.LeftAlone) == 0 && d.Compose != nil {
						for _, r := range owned.Resources {
							if r.Kind == "network" {
								projection.networks = append(projection.networks, r.Name)
							}
						}
					}
					pruneOpts.teardown = projection
				}
				pruned, err := pruneCache(ctx, d, opts.Cache, sink, pruneOpts)
				report.Pruned = pruned
				return err
			},
		})
	}
	return report, steps.Run(ctx, d.Sink, plan, steps.Options{})
}

// downStep removes the stack's helper containers, then runs compose down,
// even if a helper couldn't be removed. A stack that was never rendered
// has no compose file, and no containers compose started.
func downStep(d *Deps, st *stack.Stack, name string, owned *Ownership, report *TeardownReport) steps.Step {
	return steps.Step{
		ID:    StepTeardownDown,
		Title: "Stop and remove the stack's containers",
		Apply: func(ctx context.Context, sink events.Sink) error {
			helpers, helperErr := selectHelperContainers(ctx, d, st, name, nil)
			if report.DryRun {
				if helperErr != nil {
					return helperErr
				}
				for _, c := range helpers {
					report.Containers = append(report.Containers, c.Names)
				}
				if d.Compose != nil {
					for _, r := range owned.Resources {
						if r.Kind == "container" {
							report.Containers = append(report.Containers, r.Name)
						}
					}
				}
				for _, name := range report.Containers {
					sink.Emit(events.Progress{ID: StepTeardownDown, Text: "would remove container " + name})
				}
				return nil
			}
			if helperErr == nil {
				helperErr = removeHelperContainers(ctx, d, sink, StepTeardownDown, helpers)
			}
			if d.Compose == nil {
				sink.Emit(events.Progress{ID: StepTeardownDown, Text: "the stack was never rendered, so compose started nothing"})
				return helperErr
			}
			out := events.NewLogWriter(sink, StepTeardownDown, events.StreamStderr)
			err := d.Compose.Down(ctx, docker.ComposeDownOpts{Out: out})
			_ = out.Close()
			return errors.Join(helperErr, err)
		},
	}
}

// RemoveHelperContainers removes, with a warning each, the containers
// labelled for stack name that compose didn't start and the ownership rule
// calls st's (or adopts): helpers that a killed or interrupted command
// left, such as a dictionary-etl, which holds the data network and
// hpds-data. With match, only those whose name it accepts. Another stack's
// (a copy's original) are never touched.
func RemoveHelperContainers(ctx context.Context, d *Deps, sink events.Sink, step string, st *stack.Stack, name string, match func(container string) bool) error {
	cs, err := selectHelperContainers(ctx, d, st, name, match)
	if err != nil {
		return err
	}
	return removeHelperContainers(ctx, d, sink, step, cs)
}

func selectHelperContainers(ctx context.Context, d *Deps, st *stack.Stack, name string, match func(string) bool) ([]psContainer, error) {
	cs, err := helperContainers(ctx, d, name)
	if err != nil {
		return nil, err
	}
	dir := CanonicalDir(st.Dir)
	return slices.DeleteFunc(cs, func(c psContainer) bool {
		return match != nil && !match(c.Names) || stack.Owner(st.ID(), dir, c.Labels) == stack.Foreign
	}), nil
}

func removeHelperContainers(ctx context.Context, d *Deps, sink events.Sink, step string, cs []psContainer) error {
	var failed []string
	for _, c := range cs {
		if err := d.Docker.Rm(ctx, c.Names, true); err != nil {
			failed = append(failed, c.Names)
			sink.Emit(events.Warning{ID: step, Text: fmt.Sprintf("couldn't remove container %s, which an earlier run left: %v", c.Names, err)})
			continue
		}
		sink.Emit(events.Warning{ID: step, Text: "removed container " + c.Names + ", which an earlier run left"})
	}
	if len(failed) > 0 {
		return fmt.Errorf("couldn't remove containers %s", strings.Join(failed, ", "))
	}
	return nil
}

// volumesStep removes the stack's volumes in owned that remove selects.
// Selection is by label only, never by name (§4), and a shared data set or
// host volume is never the stack's, whatever its labels say.
func volumesStep(d *Deps, owned *Ownership, report *TeardownReport, remove func(catalog.Volume) bool) steps.Step {
	return steps.Step{
		ID:    StepVolumes,
		Title: "Remove the stack's volumes",
		Apply: func(ctx context.Context, sink events.Sink) error {
			selected, kept := selectVolumes(owned, remove)
			report.KeptVolumes = append(report.KeptVolumes, kept...)
			var failed []string
			for _, name := range selected {
				if report.DryRun {
					report.Volumes = append(report.Volumes, name)
					sink.Emit(events.Progress{ID: StepVolumes, Text: "would remove volume " + name})
					continue
				}
				if err := d.Docker.VolumeRemove(ctx, name); err != nil {
					failed = append(failed, name)
					sink.Emit(events.Warning{ID: StepVolumes, Text: fmt.Sprintf("couldn't remove volume %s: %v", name, err)})
					continue
				}
				report.Volumes = append(report.Volumes, name)
				sink.Emit(events.Progress{ID: StepVolumes, Text: "removed volume " + name})
			}
			if len(failed) > 0 {
				return fmt.Errorf("couldn't remove volumes %s", strings.Join(failed, ", "))
			}
			return nil
		},
	}
}

func selectVolumes(owned *Ownership, remove func(catalog.Volume) bool) (selected, kept []string) {
	for _, v := range owned.Resources {
		if v.Kind != "volume" || v.Claim == stack.Foreign {
			continue
		}
		cv, known := catalog.LookupVolume(v.Labels[stack.LabelComposeVolume])
		if known && cv.Scope != catalog.StackScoped {
			continue
		}
		if remove(cv) {
			selected = append(selected, v.Name)
		} else {
			kept = append(kept, v.Name)
		}
	}
	return selected, kept
}

// removeDevImages removes the dev-<name>-* images of the built catalog
// images that are st's: labelled for it by the ownership rule, or from
// before dev images had stack labels.
func removeDevImages(ctx context.Context, d *Deps, sink events.Sink, st *stack.Stack, name string, report *TeardownReport) error {
	selected, err := selectDevImages(ctx, d, st, name)
	if err != nil {
		return err
	}
	var failed []string
	for _, image := range selected {
		ref := image.ref
		if image.owner != "" {
			sink.Emit(events.Warning{ID: StepDevImages, Text: fmt.Sprintf("left image %s alone: it belongs to %s", ref, image.owner)})
			continue
		}
		if report.DryRun {
			report.Images = append(report.Images, ref)
			sink.Emit(events.Progress{ID: StepDevImages, Text: "would remove image " + ref})
			continue
		}
		if err := d.Docker.RemoveImage(ctx, ref); err != nil {
			failed = append(failed, ref)
			sink.Emit(events.Warning{ID: StepDevImages, Text: fmt.Sprintf("couldn't remove image %s: %v", ref, err)})
			continue
		}
		report.Images = append(report.Images, ref)
		sink.Emit(events.Progress{ID: StepDevImages, Text: "removed image " + ref})
	}
	if len(failed) > 0 {
		return fmt.Errorf("couldn't remove images %s", strings.Join(failed, ", "))
	}
	return nil
}

type devImageSelection struct {
	ref   string
	owner string
}

func selectDevImages(ctx context.Context, d *Deps, st *stack.Stack, name string) ([]devImageSelection, error) {
	imgs, err := d.Docker.ImageList(ctx, catalog.Namespace+"/*")
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	id := st.ID()
	var selected []devImageSelection
	for _, img := range imgs {
		if devStack, ok := cachedImage(img.Ref); !ok || devStack != name {
			continue
		}
		if _, labelled := img.Labels[stack.LabelStackDir]; labelled && stack.Owner(id, st.Dir, img.Labels) == stack.Foreign {
			selected = append(selected, devImageSelection{ref: img.Ref, owner: stack.OwnerName(img.Labels)})
			continue
		}
		selected = append(selected, devImageSelection{ref: img.Ref})
	}
	return selected, nil
}
