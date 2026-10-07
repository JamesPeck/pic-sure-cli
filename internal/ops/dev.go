package ops

import (
	"context"
	"slices"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Dev's step IDs. Dev also runs the image, render and restart steps.
const (
	DevConfigStepID = "dev-config"
	DevStartStepID  = "dev-start"
)

// DevOptions configure DevSteps.
type DevOptions struct {
	ConvergeOptions
	// Variant is the dev variant (catalog.DevVariants) to switch.
	Variant string
	// On adds the variant to dev.services; otherwise it is removed.
	On bool
}

// DevVariantInfo is one row of `dev list`.
type DevVariantInfo struct {
	Name      string   `json:"name"`
	Component string   `json:"component"`
	Services  []string `json:"services"`
	// Port is the host port the variant publishes on 127.0.0.1 (JDWP, or
	// Vite for httpd-hmr), 0 for none.
	Port int  `json:"port"`
	On   bool `json:"on"`
	// Source is components.<component>.source, which dev on needs.
	Source string `json:"source"`
}

// DevList returns every dev variant with its port and state in cfg.
func DevList(cfg *stack.Config) []DevVariantInfo {
	out := []DevVariantInfo{}
	for _, v := range catalog.DevVariants() {
		out = append(out, DevVariantInfo{
			Name:      v.Name,
			Component: v.Component,
			Services:  v.Services,
			Port:      DevPort(cfg, v),
			On:        slices.Contains(cfg.Dev.Services, v.Name),
			Source:    componentSource(cfg, v.Component),
		})
	}
	return out
}

// DevPort is the host port v publishes in cfg's stack, 0 if none.
func DevPort(cfg *stack.Config, v catalog.DevVariant) int {
	if v.Port == catalog.NoPort {
		return 0
	}
	return cfg.Network.DevPorts.Base + v.Port
}

// ComponentSource is components.<name>.source.
func ComponentSource(cfg *stack.Config, name string) string { return componentSource(cfg, name) }

// LookupDev returns the dev variant name, or exit 2 listing them.
func LookupDev(name string) (catalog.DevVariant, error) {
	v, ok := catalog.LookupDevVariant(name)
	if !ok {
		var names []string
		for _, v := range catalog.DevVariants() {
			names = append(names, v.Name)
		}
		return v, exitcode.Usage("no dev variant %q; want one of %s", name, strings.Join(names, ", "))
	}
	return v, nil
}

// CheckDevOn refuses `dev on v` when the variant's component has no local
// source, or when it would run beside the other variant that replaces
// httpd. Both are exit 3.
func CheckDevOn(cfg *stack.Config, v catalog.DevVariant) error {
	if componentSource(cfg, v.Component) == "" {
		return exitcode.Precondition("dev %s builds %s from a local checkout; set it first: pic-sure config set components.%s.source PATH",
			v.Name, v.Component, v.Component)
	}
	for _, other := range cfg.Dev.Services {
		o, ok := catalog.LookupDevVariant(other)
		if ok && o.Name != v.Name && slices.ContainsFunc(o.Services, func(s string) bool { return slices.Contains(v.Services, s) }) {
			return exitcode.Precondition("dev %s and dev %s both replace %s; run pic-sure dev off %s first",
				v.Name, o.Name, strings.Join(o.Services, ", "), o.Name)
		}
	}
	return nil
}

// DevSteps switch a dev variant on or off on an initialised stack (§7.3).
//
// On: the image step builds the variant's component from its local
// source and records the dev builds in state.json's dev_images; render
// adds the dev fragment (dev image, debug port on 127.0.0.1, JDWP agent);
// dev-config then saves dev.services, so a failed build or render leaves
// pic-sure.yaml as it was; up's restart step restarts what reads a changed
// rendered file; and dev-start recreates what changed.
//
// Off: render, restart and dev-start as above, then dev-config, last so
// that a failed run can be retried; it also drops the variant's
// dev_images. The services keep running the source build while the
// component's source is set.
//
// cfg is doc decoded. DevSteps sets cfg.Dev.Services at once; dev-config
// writes it to doc.
func DevSteps(d *Deps, st *stack.Stack, doc *stack.ConfigDoc, cfg *stack.Config, state *stack.State, opts DevOptions) ([]steps.Step, error) {
	v, err := LookupDev(opts.Variant)
	if err != nil {
		return nil, err
	}
	services := slices.DeleteFunc(slices.Clone(cfg.Dev.Services), func(s string) bool { return s == v.Name })
	if opts.On {
		services = append(services, v.Name)
	}
	cfg.Dev.Services = services
	if v.Image != "" && opts.On && state.Images[v.Image] == "" {
		return nil, exitcode.Precondition("dev %s needs the %s image from the %s source's .nvmrc, which this pic-sure doesn't set up yet", v.Name, v.Image, v.Component)
	}
	r := &upRestarts{d: d, st: st, cfg: cfg, opts: opts.ConvergeOptions}
	save := steps.Step{
		ID:    DevConfigStepID,
		Title: "Save dev.services",
		Apply: func(context.Context, events.Sink) error {
			return saveDevServices(st, doc, state, v, services, opts.On)
		},
	}
	list := []steps.Step{r.watchRender(RenderStep(d, st, cfg, state, opts.ConvergeOptions))}
	if opts.On {
		images := ImagesStep(d, st, cfg, state, ImagesOptions{Cache: opts.Cache, Components: []string{v.Component}})
		list = append([]steps.Step{images}, append(list, save)...)
	}
	list = append(list,
		withCompose(d, opts.ConvergeOptions, r.step()),
		withCompose(d, opts.ConvergeOptions, devStartStep(d, cfg, v)),
	)
	if !opts.On {
		list = append(list, save)
	}
	return list, nil
}

// saveDevServices writes dev.services to pic-sure.yaml and, when the
// variant goes off, drops its images from state.json's dev_images, which
// the image step would otherwise do on the next up.
func saveDevServices(st *stack.Stack, doc *stack.ConfigDoc, state *stack.State, v catalog.DevVariant, services []string, on bool) error {
	if err := doc.SetValue("dev.services", services); err != nil {
		return err
	}
	data, err := doc.Bytes()
	if err != nil {
		return err
	}
	if err := st.WriteConfig(data); err != nil {
		return err
	}
	if on {
		return nil
	}
	fresh, err := st.LoadState()
	if err != nil {
		return err
	}
	for _, svc := range v.Services {
		if s, ok := catalog.LookupService(svc); ok {
			delete(fresh.DevImages, s.Image)
		}
	}
	if err := st.SaveState(fresh); err != nil {
		return err
	}
	*state = *fresh
	return nil
}

// devStartStep is dev-start. On a running stack it runs `compose up -d
// --no-deps --wait` for the variant's services and the running services
// built from its component, which a rebuild from the source may have
// changed even under the same tag (a dirty checkout's); compose recreates
// only those whose config or image changed. On a stopped stack it leaves
// the new render to up.
func devStartStep(d *Deps, cfg *stack.Config, v catalog.DevVariant) steps.Step {
	return steps.Step{
		ID:    DevStartStepID,
		Title: "Recreate " + strings.Join(v.Services, ", "),
		Apply: func(ctx context.Context, sink events.Sink) error {
			ps, err := d.Compose.Ps(ctx)
			if err != nil {
				return err
			}
			running := map[string]bool{}
			for _, c := range ps {
				if c.State == "running" {
					running[c.Service] = true
				}
			}
			if len(running) == 0 {
				sink.Emit(events.Warning{ID: DevStartStepID, Text: "the stack isn't running; pic-sure up starts it with this change"})
				return nil
			}
			var svcs []string
			for _, svc := range StartServices(cfg) {
				s, _ := catalog.LookupService(svc)
				img, _ := catalog.LookupImage(s.Image)
				if slices.Contains(v.Services, svc) || running[svc] && img.Component == v.Component {
					svcs = append(svcs, svc)
				}
			}
			out := events.NewLogWriter(sink, DevStartStepID, events.StreamStderr)
			defer func() { _ = out.Close() }()
			return d.Compose.Up(ctx, docker.ComposeUpOpts{
				Services:    svcs,
				NoDeps:      true,
				Wait:        true,
				WaitTimeout: StartWaitTimeout,
				Out:         out,
			})
		},
	}
}
