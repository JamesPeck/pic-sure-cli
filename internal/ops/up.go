package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// RestartStepID is the ID of up's last step, which restarts the services
// that were running on files or volumes the earlier steps changed.
const RestartStepID = "restart"

const httpd = "httpd"

// UpStepIDs are the IDs of UpSteps for a stack with config cfg, in order,
// so up can check --skip-step before it takes the lock.
func UpStepIDs(cfg *stack.Config) []string {
	ids := []string{ImagesStepID, TLSStepID, TruststoreStepID, RenderStepID, StepDB}
	if cfg.DB.Mode == stack.DBRemote {
		ids = append(ids, StepDBBootstrap)
	}
	return append(ids, StepMigrate, StepSeed, HPDSKeyStepID, StartStepID, RestartStepID)
}

// UpSteps are §9.2 for an initialised stack: the images (built if
// missing), TLS and the truststore, a fresh render, then ConvergeSteps,
// each skipped when its Check finds it done. On a running stack whose
// images, files and data are current, `compose up` changes nothing, so up
// only verifies.
//
// Neither the TLS nor the truststore step restarts what reads its volume,
// and compose doesn't recreate a container whose rendered files changed
// but whose compose config didn't. So when one of them applies, or the
// render changes a file under render/files, the services that were
// running on the old files before up are restarted by the last step,
// "restart", once the stack is up.
func UpSteps(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts ConvergeOptions) []steps.Step {
	r := &upRestarts{d: d, st: st, opts: opts}
	list := []steps.Step{
		ImagesStep(d, st, cfg, state, ImagesOptions{Cache: opts.Cache}),
		r.restartAfter(TLSStep(d, st, cfg), httpd),
		r.restartAfter(StackTruststoreStep(d, st, cfg, state), psama),
		r.watchRender(RenderStep(d, st, cfg, state, opts)),
	}
	list = append(list, ConvergeSteps(d, st, cfg, sec, opts)...)
	return append(list, withCompose(d, opts, r.step()))
}

// upRestarts collects the services up must restart after `compose up`.
type upRestarts struct {
	d    *Deps
	st   *stack.Stack
	opts ConvergeOptions

	// running is the set of services running before up changed anything,
	// read once, when first needed; nil until then.
	running map[string]bool
	// services were running and read a volume a step refilled.
	services []string
	// changed are the absolute paths of the rendered files the render
	// step changed, added or removed.
	changed []string
}

// wasRunning reports whether service was running before up changed
// anything. If compose can't say, it assumes it was, so a needless
// restart is the worst outcome.
func (r *upRestarts) wasRunning(ctx context.Context, service string) bool {
	if r.running == nil {
		r.running = map[string]bool{}
		svcs, err := r.ps(ctx)
		if err != nil {
			r.running = nil
			return true
		}
		for _, s := range svcs {
			if s.State == "running" {
				r.running[s.Service] = true
			}
		}
	}
	return r.running[service]
}

// ps lists the stack's containers through the adapter the steps use, or
// one over the current render when they have none yet.
func (r *upRestarts) ps(ctx context.Context) ([]docker.ComposeService, error) {
	c := r.d.Compose
	if c == nil {
		if r.opts.Compose == nil {
			return nil, errors.New("no compose adapter for the stack")
		}
		var err error
		if c, err = r.opts.Compose(); err != nil {
			return nil, err
		}
	}
	return c.Ps(ctx)
}

// restartAfter makes s, when it applies, mark service for a restart if it
// was running.
func (r *upRestarts) restartAfter(s steps.Step, service string) steps.Step {
	apply := s.Apply
	s.Apply = func(ctx context.Context, sink events.Sink) error {
		running := r.wasRunning(ctx, service)
		if err := apply(ctx, sink); err != nil {
			return err
		}
		if running && !slices.Contains(r.services, service) {
			r.services = append(r.services, service)
		}
		return nil
	}
	return s
}

// watchRender makes the render step record which files under render/files
// it changed. The running services are read first, while d.Compose still
// reads the old render.
func (r *upRestarts) watchRender(s steps.Step) steps.Step {
	apply := s.Apply
	s.Apply = func(ctx context.Context, sink events.Sink) error {
		r.wasRunning(ctx, httpd)
		dir := r.st.Path(render.FilesDir)
		before, err := readTree(dir)
		if err != nil {
			return err
		}
		if err := apply(ctx, sink); err != nil {
			return err
		}
		after, err := readTree(dir)
		if err != nil {
			return err
		}
		for p, data := range after {
			if old, ok := before[p]; !ok || !bytes.Equal(old, data) {
				r.changed = append(r.changed, p)
			}
		}
		for p := range before {
			if _, ok := after[p]; !ok {
				r.changed = append(r.changed, p)
			}
		}
		slices.Sort(r.changed)
		return nil
	}
	return s
}

// readTree reads every regular file under dir, keyed by absolute path. A
// missing dir is empty.
func readTree(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipDir
			}
			return err
		}
		if !e.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = data
		return nil
	})
	return out, err
}

// pending returns the services to restart: those marked by a step, and
// the running ones that bind-mount a changed rendered file or a directory
// holding one, sorted.
func (r *upRestarts) pending(ctx context.Context) ([]string, error) {
	want := map[string]bool{}
	for _, s := range r.services {
		want[s] = true
	}
	if len(r.changed) > 0 {
		mounts, err := bindSources(ctx, r.d)
		if err != nil {
			return nil, err
		}
		for svc, sources := range mounts {
			if want[svc] || !r.wasRunning(ctx, svc) {
				continue
			}
			for _, src := range sources {
				if slices.ContainsFunc(r.changed, func(p string) bool { return within(p, src) }) {
					want[svc] = true
					break
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(want)), nil
}

// bindSources reads each service's bind mount sources from `compose
// config`, so overrides count.
func bindSources(ctx context.Context, d *Deps) (map[string][]string, error) {
	out, err := d.Compose.Config(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("reading the compose config: %w", err)
	}
	var doc struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `yaml:"type"`
				Source string `yaml:"source"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("reading the compose config: %w", err)
	}
	mounts := map[string][]string{}
	for name, svc := range doc.Services {
		for _, v := range svc.Volumes {
			if v.Type == "bind" {
				mounts[name] = append(mounts[name], v.Source)
			}
		}
	}
	return mounts, nil
}

// step is up's restart step. It is done when no step marked a service and
// the render changed no file. A
// failed restart is a warning naming the command to run, since a re-run
// would find nothing changed and skip the step.
func (r *upRestarts) step() steps.Step {
	return steps.Step{
		ID:    RestartStepID,
		Title: "Restart services on changed files",
		Check: func(context.Context) (bool, error) {
			return len(r.services) == 0 && len(r.changed) == 0, nil
		},
		Apply: func(ctx context.Context, sink events.Sink) error {
			svcs, err := r.pending(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return err
				}
				sink.Emit(events.Warning{ID: RestartStepID, Text: "can't tell which services read the changed files: " + err.Error() +
					"; run `pic-sure restart` if the stack misbehaves"})
				return nil
			}
			if len(svcs) == 0 {
				return nil
			}
			sink.Emit(events.Progress{ID: RestartStepID, Text: "restarting " + strings.Join(svcs, ", ") + " to pick up the changed files"})
			out := events.NewLogWriter(sink, RestartStepID, events.StreamStderr)
			defer func() { _ = out.Close() }()
			if err := r.d.Compose.Restart(ctx, out, svcs...); err != nil {
				if ctx.Err() != nil {
					return err
				}
				sink.Emit(events.Warning{ID: RestartStepID, Text: "restarting " + strings.Join(svcs, ", ") + " failed: " + err.Error() +
					"; run `pic-sure restart " + strings.Join(svcs, " ") + "`"})
			}
			return nil
		},
	}
}
