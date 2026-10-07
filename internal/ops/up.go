package ops

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// RestartStepID is the ID of up's restart step, which restarts the
// running services whose files or volumes the earlier steps changed.
const RestartStepID = "restart"

const httpd = "httpd"

// UpStepIDs are the IDs of UpSteps for a stack with config cfg, in order,
// so up can check --skip-step before it takes the lock.
func UpStepIDs(cfg *stack.Config) []string {
	return append([]string{ResolveStepID}, upStepIDs(cfg)...)
}

// upStepIDs are the IDs of upSteps.
func upStepIDs(cfg *stack.Config) []string {
	ids := []string{ImagesStepID}
	if hmrOn(cfg) {
		ids = append(ids, NodeImageStepID)
	}
	ids = append(ids, TLSStepID, TruststoreStepID, RenderStepID, StepDB)
	if cfg.DB.Mode == stack.DBRemote {
		ids = append(ids, StepDBBootstrap)
	}
	ids = append(ids, StepMigrate, StepSeed, HPDSKeyStepID, RestartStepID)
	if hmrOn(cfg) && HostUser() != "" {
		ids = append(ids, HMRVolumeStepID)
	}
	return append(ids, StartStepID)
}

// UpSteps are §9.2 for an initialised stack: resolve the components whose
// local source the config no longer sets, the images (built if missing),
// TLS and the truststore, a fresh render, then ConvergeSteps with a restart
// step before start, each skipped when its Check finds it done. On a
// running stack whose images, files and data are current, `compose up`
// changes nothing, so up only verifies.
//
// A component whose source was unset goes back to the release (§7.3): it
// is resolved at the release commit state.json records, without moving
// that or any other component, so its release images are built or pulled
// and start recreates its services on them.
//
// Neither the TLS nor the truststore step restarts what reads its volume,
// and compose doesn't recreate a container whose rendered files changed
// but whose compose config didn't. So when one of them applies, or the
// render changes a file under render/files, the services that read it are
// recorded in state.json's PendingRestarts, and the restart step restarts
// those that are running before start waits for the stack to be healthy.
func UpSteps(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts ConvergeOptions) []steps.Step {
	resolve := resolveStep(d, st, cfg, state, opts.Cache, unsetSources)
	apply := resolve.Apply
	resolve.Apply = func(ctx context.Context, sink events.Sink) error {
		// Resolving without one would move the stack to the branch head.
		if state.Release.Commit == "" {
			return exitcode.Precondition("state.json records no release commit; run pic-sure build")
		}
		return apply(ctx, sink)
	}
	images := ImagesStep(d, st, cfg, state, ImagesOptions{Cache: opts.Cache})
	return append([]steps.Step{resolve}, upSteps(d, st, cfg, sec, state, opts, images)...)
}

// upSteps are UpSteps with images as the image step, which update
// configures differently.
func upSteps(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts ConvergeOptions, images steps.Step) []steps.Step {
	r := &upRestarts{d: d, st: st, cfg: cfg, opts: opts}
	converge := ConvergeSteps(d, st, cfg, sec, opts)
	last := len(converge) - 1
	list := []steps.Step{images}
	if hmrOn(cfg) {
		list = append(list, NodeImageStep(st, cfg, state))
	}
	list = append(list,
		r.restartAfter(TLSStep(d, st, cfg), httpd),
		r.restartAfter(StackTruststoreStep(d, st, cfg, state), psama),
		r.watchRender(RenderStep(d, st, cfg, state, opts)),
	)
	list = append(list, converge[:last]...)
	list = append(list, withCompose(d, opts, r.step()))
	if user := HostUser(); hmrOn(cfg) && user != "" {
		list = append(list, HMRVolumeStep(d, st, cfg, user))
	}
	return append(list, converge[last])
}

// upRestarts records and runs the restarts up's steps call for.
type upRestarts struct {
	d    *Deps
	st   *stack.Stack
	cfg  *stack.Config
	opts ConvergeOptions
}

// mark adds services to state.json's PendingRestarts.
func (r *upRestarts) mark(services ...string) error {
	return markPendingRestarts(r.st, services...)
}

// restartAfter makes s mark service for a restart before it applies, so
// no failure or interruption of s can lose the restart.
func (r *upRestarts) restartAfter(s steps.Step, service string) steps.Step {
	apply := s.Apply
	s.Apply = func(ctx context.Context, sink events.Sink) error {
		if err := r.mark(service); err != nil {
			return err
		}
		return apply(ctx, sink)
	}
	return s
}

// watchRender makes the render step mark the services that bind-mount a
// file under render/files it changed, added or removed, or a directory
// holding one. It does so even when the render fails partway, since the
// re-run would find the files it wrote unchanged.
func (r *upRestarts) watchRender(s steps.Step) steps.Step {
	apply := s.Apply
	s.Apply = func(ctx context.Context, sink events.Sink) error {
		dir := r.st.Path(render.FilesDir)
		before, err := readTree(dir)
		if err != nil {
			return err
		}
		applyErr := apply(ctx, sink)
		after, err := readTree(dir)
		if err != nil {
			return errors.Join(applyErr, err)
		}
		var changed []string
		for p, data := range after {
			if old, ok := before[p]; !ok || !bytes.Equal(old, data) {
				changed = append(changed, p)
			}
		}
		for p := range before {
			if _, ok := after[p]; !ok {
				changed = append(changed, p)
			}
		}
		if len(changed) == 0 {
			return applyErr
		}
		readers, err := r.readers(ctx, changed)
		if err != nil {
			// The files are written, so a re-run would see no change:
			// restart everything rather than lose the restart.
			sink.Emit(events.Warning{ID: RenderStepID, Text: "can't tell which services read the changed files (" + err.Error() +
				"); restarting every running service"})
			readers = StartServices(r.cfg)
		}
		return errors.Join(applyErr, r.mark(readers...))
	}
	return s
}

// readers returns the services that bind-mount one of paths, or a
// directory holding one, per `compose config` over the new render.
func (r *upRestarts) readers(ctx context.Context, paths []string) ([]string, error) {
	if err := ensureCompose(r.d, r.opts); err != nil {
		return nil, err
	}
	mounts, err := bindMounts(ctx, r.d.Compose)
	if err != nil {
		return nil, err
	}
	var out []string
	for svc, m := range mounts {
		for _, src := range m {
			if slices.ContainsFunc(paths, func(p string) bool { return within(p, src) }) {
				out = append(out, svc)
				break
			}
		}
	}
	slices.Sort(out)
	return out, nil
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

// step is up's restart step. It is done when nothing is pending. It
// restarts the pending services that are running; compose up starts the
// others on the new files. They come off the list only once restarted.
func (r *upRestarts) step() steps.Step {
	pending := func() ([]string, error) {
		state, err := r.st.LoadState()
		if err != nil {
			return nil, err
		}
		return state.PendingRestarts, nil
	}
	return steps.Step{
		ID:    RestartStepID,
		Title: "Restart services on changed files",
		Check: func(context.Context) (bool, error) {
			svcs, err := pending()
			return len(svcs) == 0, err
		},
		Apply: func(ctx context.Context, sink events.Sink) error {
			svcs, err := pending()
			if err != nil {
				return err
			}
			running, err := r.d.Compose.Ps(ctx)
			if err != nil {
				return err
			}
			var restart []string
			for _, s := range svcs {
				if slices.ContainsFunc(running, func(c docker.ComposeService) bool { return c.Service == s && c.State == "running" }) {
					restart = append(restart, s)
				}
			}
			if len(restart) > 0 {
				sink.Emit(events.Progress{ID: RestartStepID, Text: "restarting " + strings.Join(restart, ", ") + " to pick up the changed files"})
				out := events.NewLogWriter(sink, RestartStepID, events.StreamStderr)
				err := r.d.Compose.Restart(ctx, out, restart...)
				_ = out.Close()
				if err != nil {
					return err
				}
			}
			state, err := r.st.LoadState()
			if err != nil {
				return err
			}
			state.PendingRestarts = slices.DeleteFunc(state.PendingRestarts, func(s string) bool { return slices.Contains(svcs, s) })
			return r.st.SaveState(state)
		},
	}
}
