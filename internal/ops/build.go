package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Step IDs of the build command. ImagesStepID is also the image step of
// init, up and update.
const (
	ResolveStepID = "resolve"
	ImagesStepID  = "images"
)

// BuildLogDir is where the image step writes each build's log, one file per
// part (§7.2 step 6), replaced by the next build of that part.
const BuildLogDir = ".pic-sure/logs/build"

// DefaultRegistry is where pull mode pulls from when images.registry is
// empty (D33).
const DefaultRegistry = "ghcr.io/hms-dbmi"

// Image actions in a BuildReport.
const (
	ImageBuilt    = "built"
	ImagePulled   = "pulled"
	ImageUpToDate = "up-to-date"
)

// ImagesOptions configures the image step.
type ImagesOptions struct {
	// Cache holds the source trees, the build directories and the locks.
	Cache *cache.Cache
	// Components limits the step to these components (catalog names).
	// Empty means all of them.
	Components []string
	// Force rebuilds every image even if it is up to date. Pull mode pulls
	// every time anyway.
	Force bool
}

// BuildOptions configures Build, the build command.
type BuildOptions struct {
	ImagesOptions
	// SkipSteps are the --skip-step IDs.
	SkipSteps []string
}

// BuildReport is what the image step did.
type BuildReport struct {
	Images []BuiltImage `json:"images"`
}

// BuiltImage is one image the step built, pulled or found up to date.
type BuiltImage struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	Ref       string `json:"ref"`
	Action    string `json:"action"`
	// Source is the local checkout it was built from (§7.3), if any.
	Source string `json:"source,omitempty"`
}

// imagePart is one unit of the image step: the reactor's images, the
// frontend or dictionary-etl.
type imagePart struct {
	component string
	images    []catalog.Image
	comp      stack.Component // with Source and Dirty for a local checkout
	tag       string
	pull      bool // pulled from the registry rather than built
	registry  string
	// tree says render needs the cache's source tree, for its SQL and
	// schema bind mounts, whether or not anything is built from it.
	tree bool
}

// Build is the build command: it resolves the component commits if
// state.json lacks them, then runs the image step, which reports on every
// image of the selected components.
func Build(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts BuildOptions) (*BuildReport, error) {
	report := &BuildReport{Images: []BuiltImage{}}
	images := imagesStep(d, st, cfg, state, opts.ImagesOptions, report)
	// The builds skip what is up to date themselves, and so report every
	// image.
	images.Check = nil
	plan := []steps.Step{resolveStep(d, st, cfg, state, opts.Cache), images}
	return report, steps.Run(ctx, d.Sink, plan, steps.Options{Skip: opts.SkipSteps})
}

// resolveStep resolves the commits of the components without a local
// source that state.json has no release commit for: never resolved, or last
// built from a source the config no longer sets. They are resolved at the
// release commit state.json records, if any, and the other components are
// left as they are, since moving them is update's business.
func resolveStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, c *cache.Cache) steps.Step {
	return steps.Step{
		ID:    ResolveStepID,
		Title: "Resolve the component commits",
		Check: func(context.Context) (bool, error) {
			return len(unresolved(cfg, state)) == 0, nil
		},
		Apply: func(ctx context.Context, sink events.Sink) error {
			opts := release.Options{Repo: cfg.Release.Repo, Branch: cfg.Release.Branch, Commit: state.Release.Commit}
			if state.Release.Commit != "" && state.Release.Repo != "" {
				opts.Repo = state.Release.Repo
			}
			rel, err := release.Fetch(ctx, c, d.Git, sink, ResolveStepID, opts)
			if err != nil {
				return err
			}
			comps, err := rel.ResolveComponents(ctx, c.WithEvents(sink, ResolveStepID), sink, ResolveStepID, cfg.Components)
			if err != nil {
				return err
			}
			if state.Release.Commit == "" {
				state.Release = stack.Release{Repo: rel.Repo, Branch: rel.Branch, Commit: rel.Commit}
			}
			if state.Components == nil {
				state.Components = map[string]stack.Component{}
			}
			for _, name := range unresolved(cfg, state) {
				state.Components[name] = comps[name]
			}
			return st.SaveState(state)
		},
	}
}

// unresolved lists the components without a local source whose release
// commit state.json doesn't record.
func unresolved(cfg *stack.Config, state *stack.State) []string {
	var out []string
	for _, comp := range catalog.Components() {
		rec := state.Components[comp.Name]
		if componentSource(cfg, comp.Name) == "" && (rec.Commit == "" || rec.Source != "") {
			out = append(out, comp.Name)
		}
	}
	return out
}

// ImagesStep is §7.2's image step, ID "images", for init, up and update:
// it builds or pulls each component's images at the commit state.json
// records and records their tags in state, which it saves. A component
// with a local source (§7.3) is built from that checkout instead, tagged
// dev-<stack>-<sha12> (plus -dirty, and always rebuilt, when the checkout
// has changes). Check reports done when every image is present and
// recorded.
func ImagesStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts ImagesOptions) steps.Step {
	return imagesStep(d, st, cfg, state, opts, &BuildReport{})
}

func imagesStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts ImagesOptions, report *BuildReport) steps.Step {
	return steps.Step{
		ID:    ImagesStepID,
		Title: "Build the images",
		Check: func(ctx context.Context) (bool, error) {
			if opts.Force {
				return false, nil
			}
			parts, err := planImages(ctx, d, st, cfg, state, opts)
			if err != nil {
				return false, err
			}
			for _, p := range parts {
				if ok, err := p.upToDate(ctx, d, opts.Cache, cfg, state); err != nil || !ok {
					return false, err
				}
			}
			return true, nil
		},
		Apply: func(ctx context.Context, sink events.Sink) error {
			parts, err := planImages(ctx, d, st, cfg, state, opts)
			if err != nil {
				return err
			}
			return buildImages(ctx, d, st, cfg, state, opts, parts, report)
		},
	}
}

// planImages works out the parts of the selected components. A local
// source's checkout is read here, so its tag follows its current commit.
func planImages(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts ImagesOptions) ([]imagePart, error) {
	selected, err := SelectComponents(opts.Components)
	if err != nil {
		return nil, err
	}
	registry := cfg.Images.Registry
	if registry == "" {
		registry = DefaultRegistry
	}
	registry = strings.TrimSuffix(registry, "/")
	var parts []imagePart
	for _, name := range selected {
		p := imagePart{component: name, images: catalog.ImagesBuiltFrom(name), registry: registry}
		if src := componentSource(cfg, name); src != "" {
			if !filepath.IsAbs(src) {
				src = filepath.Join(st.Dir, src)
			}
			wt, err := d.Git.WorkTree(ctx, src)
			if err != nil {
				return nil, fmt.Errorf("components.%s.source %s: %w", name, src, err)
			}
			p.comp = stack.Component{Commit: wt.Head, Source: src, Dirty: wt.Dirty}
			p.tag = DevTag(cfg.Name, wt.Head, wt.Dirty)
			if !imageTag.MatchString(p.tag) {
				return nil, fmt.Errorf("the dev image tag %q is longer than docker allows; use a shorter stack name", p.tag)
			}
		} else {
			rec := state.Components[name]
			if !fullCommit.MatchString(rec.Commit) || rec.Source != "" {
				return nil, exitcode.Precondition("state.json records no release commit of %s; run pic-sure build", name)
			}
			p.comp = stack.Component{Ref: rec.Ref, Commit: rec.Commit}
			p.tree = name == catalog.PicSure || name == catalog.Migrations
			p.tag = p.comp.Commit[:12]
			if name == catalog.Frontend {
				p.tag += "-" + FrontendConfigHash(render.ViteEnv(cfg))[:8]
			}
			if cfg.Images.Mode == stack.ImagesPull && name != catalog.Frontend && len(p.images) > 0 {
				// The frontend bakes in its config, so it is always built (§7.4).
				p.pull = true
				p.tag = p.comp.Ref
				if !imageTag.MatchString(p.tag) {
					return nil, fmt.Errorf("images.mode pull: %s's ref %q is not an image tag", name, p.tag)
				}
			}
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// imageTag is docker's tag syntax.
var imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// DevTag is the tag of an image built from a local checkout of commit
// (§7.3): dev-<stack>-<sha12>, with -dirty when the checkout has changes.
// The stack name keeps it from being shared.
func DevTag(stackName, commit string, dirty bool) string {
	tag := "dev-" + stackName + "-" + commit[:min(12, len(commit))]
	if dirty {
		tag += "-dirty"
	}
	return tag
}

// SelectComponents checks the build command's component names against the
// catalog; none means every component, in catalog order. An unknown name is
// exit 2.
func SelectComponents(names []string) ([]string, error) {
	var all []string
	for _, c := range catalog.Components() {
		all = append(all, c.Name)
	}
	if len(names) == 0 {
		return all, nil
	}
	var out []string
	for _, n := range names {
		if !slices.Contains(all, n) {
			return nil, exitcode.Usage("unknown component %q (want one of %s)", n, strings.Join(all, ", "))
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// componentSource is components.<name>.source.
func componentSource(cfg *stack.Config, name string) string {
	switch name {
	case catalog.PicSure:
		return cfg.Components.PicSure.Source
	case catalog.Frontend:
		return cfg.Components.Frontend.Source
	case catalog.Migrations:
		return cfg.Components.Migrations.Source
	case catalog.DictionaryETL:
		return cfg.Components.DictionaryETL.Source
	}
	return ""
}

// upToDate reports whether every image of p is present and state records
// it, and the source tree render needs exists, so the step has nothing to
// do. A dirty checkout is never up to date.
func (p imagePart) upToDate(ctx context.Context, d *Deps, c *cache.Cache, cfg *stack.Config, state *stack.State) (bool, error) {
	if p.comp.Dirty || state.Components[p.component] != p.comp {
		return false, nil
	}
	if p.tree {
		dir, err := c.SourceDir(p.component, p.comp.Commit)
		if err != nil {
			return false, err
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return false, nil
		}
	}
	tag := p.tag
	for _, img := range p.images {
		if state.Images[img.Name] != tag {
			return false, nil
		}
	}
	devWant := p.devImages(cfg, tag)
	for _, img := range p.images {
		if state.DevImages[img.Name] != devWant[img.Name] {
			return false, nil
		}
	}
	switch {
	case len(p.images) == 0:
		return true, nil
	case p.pull:
		for _, img := range p.images {
			if ok, err := d.Docker.ImageExists(ctx, img.Repository()+":"+tag); err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case p.component == catalog.PicSure:
		return ReactorUpToDate(ctx, d, p.comp.Commit, tag)
	case p.component == catalog.Frontend:
		return imageHasLabels(ctx, d, p.images[0].Repository()+":"+tag, map[string]string{
			FrontendSrcLabel: p.comp.Commit, FrontendConfigLabel: FrontendConfigHash(render.ViteEnv(cfg))})
	default:
		return imageHasLabels(ctx, d, p.images[0].Repository()+":"+tag,
			map[string]string{DictionaryETLSrcLabel: p.comp.Commit})
	}
}

// devImages returns the DevImages entries p's images should have: the tag
// of its local build for each image an enabled dev variant builds from
// source, and nothing without a local source.
func (p imagePart) devImages(cfg *stack.Config, tag string) map[string]string {
	out := map[string]string{}
	if p.comp.Source == "" {
		return out
	}
	built := devImages(cfg)
	for _, img := range p.images {
		if built[img.Name] {
			out[img.Name] = tag
		}
	}
	return out
}

// buildImages builds or pulls each part in turn, recording each one in
// state, and saving it, as soon as it is done.
func buildImages(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts ImagesOptions, parts []imagePart, report *BuildReport) error {
	if opts.Cache == nil {
		return errors.New("image build: no cache")
	}
	proxy, err := netproxy.New(netproxy.Config{HTTP: cfg.Proxy.HTTP, HTTPS: cfg.Proxy.HTTPS, NoProxy: cfg.Proxy.NoProxy}, netproxy.CatalogServices())
	if err != nil {
		return err
	}
	if !proxy.Enabled() {
		proxy = nil
	}
	logDir, err := prepareBuildLogs(st, parts)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if p.tree {
			if _, err := opts.Cache.WithEvents(d.Sink, ImagesStepID).EnsureSource(ctx, p.component, p.comp.Commit); err != nil {
				return err
			}
		}
		var tag string
		var built []string
		var err error
		switch {
		case len(p.images) == 0:
			// migrations has no image; only its commit is recorded.
		case p.pull:
			tag, built, err = p.pullImages(ctx, d)
		case p.component == catalog.PicSure:
			var res ReactorResult
			res, err = BuildReactor(ctx, d, ReactorOptions{
				Cache: opts.Cache, SHA: p.comp.Commit, Source: p.comp.Source, Tag: p.tag,
				Proxy: proxy, Force: opts.Force || p.comp.Dirty, LogDir: logDir, Step: ImagesStepID,
			})
			tag, built = res.Tag, res.Built
		default:
			bo := ImageBuildOptions{
				Cache: opts.Cache, SHA: p.comp.Commit, Source: p.comp.Source, Tag: p.tag,
				Proxy: proxy, Force: opts.Force || p.comp.Dirty, LogDir: logDir, Step: ImagesStepID,
			}
			var res ImageBuildResult
			if p.component == catalog.Frontend {
				res, err = BuildFrontend(ctx, d, cfg, bo)
			} else {
				res, err = BuildDictionaryETL(ctx, d, bo)
			}
			tag = res.Tag
			if res.Built {
				built = []string{p.images[0].Name}
			}
		}
		if err != nil {
			return err
		}
		p.record(state, cfg, tag)
		if err := st.SaveState(state); err != nil {
			return err
		}
		for _, img := range p.images {
			bi := BuiltImage{Name: img.Name, Component: p.component, Ref: img.Repository() + ":" + tag,
				Action: ImageUpToDate, Source: p.comp.Source}
			if slices.Contains(built, img.Name) {
				bi.Action = ImageBuilt
				if p.pull {
					bi.Action = ImagePulled
				}
			}
			report.Images = append(report.Images, bi)
		}
	}
	return nil
}

// record stores p's commit, image tags and dev image tags in state.
func (p imagePart) record(state *stack.State, cfg *stack.Config, tag string) {
	if state.Components == nil {
		state.Components = map[string]stack.Component{}
	}
	state.Components[p.component] = p.comp
	if len(p.images) == 0 {
		return
	}
	if state.Images == nil {
		state.Images = map[string]string{}
	}
	if state.DevImages == nil {
		state.DevImages = map[string]string{}
	}
	dev := p.devImages(cfg, tag)
	for _, img := range p.images {
		state.Images[img.Name] = tag
		delete(state.DevImages, img.Name)
		if t, ok := dev[img.Name]; ok {
			state.DevImages[img.Name] = t
		}
	}
}

// pullImages pulls each image of p from the registry, every time, since a
// ref such as a branch name can move, and tags it with the local name the
// rendered compose file uses.
func (p imagePart) pullImages(ctx context.Context, d *Deps) (string, []string, error) {
	var pulled []string
	for _, img := range p.images {
		local := img.Repository() + ":" + p.tag
		remote := p.registry + "/" + img.Name + ":" + p.tag
		progressf(d.Sink, ImagesStepID, "Pulling %s", remote)
		if err := d.Docker.Pull(ctx, remote, nil); err != nil {
			if ctx.Err() != nil {
				return "", nil, err
			}
			return "", nil, fmt.Errorf("pulling %s (PIC-SURE images may not be published to %s yet; set images.mode: build to build them from source): %w", remote, p.registry, err)
		}
		if err := d.Docker.Tag(ctx, remote, local); err != nil {
			return "", nil, err
		}
		pulled = append(pulled, img.Name)
	}
	return p.tag, pulled, nil
}

// prepareBuildLogs makes BuildLogDir and a log file for every part that
// builds, through the stack so that each is in the manifest, and returns
// the directory's host path. The builds then rewrite the files.
func prepareBuildLogs(st *stack.Stack, parts []imagePart) (string, error) {
	var names []string
	for _, p := range parts {
		switch {
		case p.pull || len(p.images) == 0:
			continue
		case p.component == catalog.PicSure:
			names = append(names, ReactorContainer)
		}
		for _, img := range p.images {
			names = append(names, img.Name)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	if err := st.MkdirAll(BuildLogDir, 0o700); err != nil {
		return "", err
	}
	for _, n := range names {
		rel := BuildLogDir + "/" + n + ".log"
		if _, err := fs.Stat(st.FS(), rel); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		f, err := st.CreateFile(rel, 0o600)
		if err != nil {
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
	}
	return st.Path(BuildLogDir), nil
}
