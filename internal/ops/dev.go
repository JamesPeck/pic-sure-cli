package ops

import (
	"context"
	"fmt"
	"maps"
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
// source and records the dev builds in state.json's dev_images; dev-config
// adds the variant to dev.services in doc and saves it; render adds the dev
// fragment (dev image, debug port on 127.0.0.1, JDWP agent); up's restart
// step restarts what reads a changed rendered file; dev-start runs
// `compose up -d --no-deps --wait` for the variant's services, and for any
// other service of the component whose image the build replaced.
//
// Off: dev-config removes the variant and its dev_images entries, then
// render, restart and dev-start as above. The services keep running the
// source build while the component's source is set.
//
// cfg is doc decoded; DevSteps updates both.
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
	r := &upRestarts{d: d, st: st, cfg: cfg, opts: opts.ConvergeOptions}
	start := &devStart{d: d, cfg: cfg, v: v}

	var list []steps.Step
	if opts.On {
		images := ImagesStep(d, st, cfg, state, ImagesOptions{Cache: opts.Cache, Components: []string{v.Component}})
		apply := images.Apply
		images.Apply = func(ctx context.Context, sink events.Sink) error {
			before := maps.Clone(state.Images)
			err := apply(ctx, sink)
			start.replaced = changedImages(before, state.Images)
			return err
		}
		list = append(list, images)
	}
	list = append(list,
		steps.Step{
			ID:    DevConfigStepID,
			Title: "Save dev.services",
			Apply: func(context.Context, events.Sink) error {
				return saveDevServices(st, doc, state, v, services, opts.On)
			},
		},
		r.watchRender(RenderStep(d, st, cfg, state, opts.ConvergeOptions)),
		withCompose(d, opts.ConvergeOptions, r.step()),
		withCompose(d, opts.ConvergeOptions, start.step()),
	)
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

// changedImages are the images whose tag differs between before and after.
func changedImages(before, after map[string]string) []string {
	var out []string
	for img, tag := range after {
		if before[img] != tag {
			out = append(out, img)
		}
	}
	return out
}

// devStart is the dev-start step.
type devStart struct {
	d   *Deps
	cfg *stack.Config
	v   catalog.DevVariant
	// replaced are the images the image step retagged.
	replaced []string
}

// services are the variant's services plus the stack's other services
// that run a replaced image, in StartServices order.
func (s *devStart) services() []string {
	var out []string
	for _, svc := range StartServices(s.cfg) {
		cs, _ := catalog.LookupService(svc)
		if slices.Contains(s.v.Services, svc) || slices.Contains(s.replaced, cs.Image) {
			out = append(out, svc)
		}
	}
	return out
}

func (s *devStart) step() steps.Step {
	return steps.Step{
		ID:    DevStartStepID,
		Title: fmt.Sprintf("Recreate %s", strings.Join(s.v.Services, ", ")),
		Apply: func(ctx context.Context, sink events.Sink) error {
			svcs := s.services()
			if extra := slices.DeleteFunc(slices.Clone(svcs), func(x string) bool { return slices.Contains(s.v.Services, x) }); len(extra) > 0 {
				sink.Emit(events.Progress{ID: DevStartStepID, Text: "also recreating " + strings.Join(extra, ", ") +
					", whose images were rebuilt from components." + s.v.Component + ".source"})
			}
			out := events.NewLogWriter(sink, DevStartStepID, events.StreamStderr)
			defer func() { _ = out.Close() }()
			return s.d.Compose.Up(ctx, docker.ComposeUpOpts{
				Services:    svcs,
				NoDeps:      true,
				Wait:        true,
				WaitTimeout: StartWaitTimeout,
				Out:         out,
			})
		},
	}
}
