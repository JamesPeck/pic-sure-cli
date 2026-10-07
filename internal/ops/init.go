package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Step IDs of §9.1's steps 7 and 12. The others are the IDs of the steps
// init reuses (ResolveStepID, ImagesStepID, TLSStepID, TruststoreStepID,
// StepDB, StepDBBootstrap, StepMigrate, StepSeed, HPDSKeyStepID).
const (
	RenderStepID = "render"
	StartStepID  = "start"
)

// StartWaitTimeout bounds `compose up --wait`: the Java services take a few
// minutes to turn healthy on a laptop.
const StartWaitTimeout = 15 * time.Minute

// ConvergeOptions are what the converging steps need besides the stack.
type ConvergeOptions struct {
	// Cache holds the release's source trees, which the image step fills
	// and render bind-mounts.
	Cache *cache.Cache
	// CLIVersion is recorded in state.json by the render step.
	CLIVersion string
	// Compose returns the stack's Composer. The steps after render call it
	// when d.Compose is nil, which the render step makes it, so the
	// Composer always reads the files just rendered. Its env must be
	// computed on each call (render.ComposeEnv over the same *Secrets the
	// seed step updates), so compose up hands the gateway the token seed
	// issued.
	Compose func() (docker.Composer, error)
}

// InitStepIDs are the IDs of InitSteps, in order, so init can check
// --skip-step before it creates anything.
var InitStepIDs = []string{
	ResolveStepID, ImagesStepID, TLSStepID, TruststoreStepID, RenderStepID,
	StepDB, StepDBBootstrap, StepMigrate, StepSeed, HPDSKeyStepID, StartStepID,
}

// InitSteps are §9.1 steps 4 to 12, for init once it has written the
// config, secrets and state: resolve the component commits, build the
// images, install TLS and the truststore, render, then ConvergeSteps.
// state is updated in place as the steps record into state.json.
func InitSteps(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts ConvergeOptions) []steps.Step {
	return append([]steps.Step{
		ResolveStep(d, st, cfg, state, opts.Cache),
		ImagesStep(d, st, cfg, state, ImagesOptions{Cache: opts.Cache}),
		TLSStep(d, st, cfg),
		StackTruststoreStep(d, st, cfg, state),
		RenderStep(d, st, cfg, state, opts),
	}, ConvergeSteps(d, st, cfg, sec, opts)...)
}

// ConvergeSteps are §9.1 steps 8 to 12, which init, up and update run on a
// rendered stack: the database (DBSteps: bootstrapped too when it is
// remote), migrations, seed, the HPDS key, then `compose up -d --wait`. Each gets d.Compose from opts.Compose if it is
// nil.
func ConvergeSteps(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, opts ConvergeOptions) []steps.Step {
	list := append(DBSteps(d, cfg, sec, DBOptions{}),
		MigrateStep(d, cfg, sec, MigrateOptions{}),
		SeedStep(d, st, cfg, sec),
		HPDSKeyStep(d, st, cfg),
		StartStep(d, cfg),
	)
	for i := range list {
		list[i] = withCompose(d, opts, list[i])
	}
	return list
}

// withCompose makes s set d.Compose from opts.Compose before it runs.
func withCompose(d *Deps, opts ConvergeOptions, s steps.Step) steps.Step {
	ensure := func() error {
		if d.Compose != nil {
			return nil
		}
		if opts.Compose == nil {
			return errors.New("no compose adapter for the stack")
		}
		c, err := opts.Compose()
		if err != nil {
			return err
		}
		d.Compose = c
		return nil
	}
	check, apply := s.Check, s.Apply
	if check != nil {
		s.Check = func(ctx context.Context) (bool, error) {
			if err := ensure(); err != nil {
				return false, err
			}
			return check(ctx)
		}
	}
	s.Apply = func(ctx context.Context, sink events.Sink) error {
		if err := ensure(); err != nil {
			return err
		}
		return apply(ctx, sink)
	}
	return s
}

// ResolveStep is the build command's resolve step, for init: it records
// the component commits of the release state.json names, which init sets to
// the release it fetched and gated. It is done when every component without
// a local source has a release commit.
func ResolveStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, c *cache.Cache) steps.Step {
	return resolveStep(d, st, cfg, state, c)
}

// StackTruststoreStep is TruststoreStep for the psama image state.json
// records when the step runs, so it can follow the image step in one plan.
func StackTruststoreStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State) steps.Step {
	inner := func() steps.Step { return TruststoreStep(d, st, cfg, psamaImage(state)) }
	s := inner()
	s.Check = func(ctx context.Context) (bool, error) { return inner().Check(ctx) }
	s.Apply = func(ctx context.Context, sink events.Sink) error { return inner().Apply(ctx, sink) }
	return s
}

// psamaImage is the psama image the stack runs: its dev build's while a dev
// variant replaces it, else the recorded release image.
func psamaImage(state *stack.State) string {
	svc, _ := catalog.LookupService(psama)
	img, _ := catalog.LookupImage(svc.Image)
	tag := state.DevImages[img.Name]
	if tag == "" {
		tag = state.Images[img.Name]
	}
	return img.Repository() + ":" + tag
}

// RenderStep is §9.1 step 7, ID "render": it renders the stack (§6.4) from
// the config and the tags state.json records, writes the files, and records
// the CLI version and config schema in state.json. It always applies, since
// rendering is cheap and the files must match this pic-sure. It leaves
// d.Compose nil, so the next step builds an adapter over the new files.
func RenderStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts ConvergeOptions) steps.Step {
	return steps.Step{
		ID:    RenderStepID,
		Title: "Render the compose file",
		Apply: func(ctx context.Context, sink events.Sink) error {
			// Steps since the image step saved state.json themselves.
			fresh, err := st.LoadState()
			if err != nil {
				return err
			}
			sources := map[string]string{}
			for _, comp := range []string{catalog.PicSure, catalog.Migrations} {
				if componentSource(cfg, comp) != "" {
					continue
				}
				commit := fresh.Components[comp].Commit
				if commit == "" {
					return exitcode.Precondition("state.json records no commit of %s; run pic-sure build", comp)
				}
				if sources[comp], err = opts.Cache.SourceDir(comp, commit); err != nil {
					return err
				}
			}
			certs, err := CustomCerts(st, cfg)
			if err != nil {
				return err
			}
			files, err := render.Render(render.Input{
				StackDir:    st.Dir,
				Config:      cfg,
				State:       fresh,
				Sources:     sources,
				CustomTrust: len(certs) > 0,
			})
			if err != nil {
				return err
			}
			if err := render.Write(st, files); err != nil {
				return err
			}
			fresh.CLIVersion, fresh.SchemaVersion = opts.CLIVersion, stack.ConfigSchema
			if err := st.SaveState(fresh); err != nil {
				return err
			}
			*state = *fresh
			d.Compose = nil
			return nil
		},
	}
}

// StartStep is §9.1 step 12, ID "start": `compose up -d --wait` for the
// stack's long-running services, which returns once they are healthy.
// Compose leaves running containers whose config hasn't changed alone, so
// on a running stack it only verifies.
func StartStep(d *Deps, cfg *stack.Config) steps.Step {
	return steps.Step{
		ID:    StartStepID,
		Title: "Start the stack",
		Apply: func(ctx context.Context, sink events.Sink) error {
			out := events.NewLogWriter(sink, StartStepID, events.StreamStderr)
			defer func() { _ = out.Close() }()
			return d.Compose.Up(ctx, docker.ComposeUpOpts{
				Services:    StartServices(cfg),
				Wait:        true,
				WaitTimeout: StartWaitTimeout,
				Out:         out,
			})
		},
	}
}

// StartServices are the services `up -d --wait` starts: every service of
// the stack's mode that isn't a one-shot. Naming them keeps the Flyway
// one-shots (profile migrate) and the shared-HPDS seed, which exit, out of
// --wait.
func StartServices(cfg *stack.Config) []string {
	mode := catalog.Mode{RemoteDB: cfg.DB.Mode == stack.DBRemote, SharedHPDS: cfg.HPDS.Data == stack.HPDSShared}
	var out []string
	for _, s := range catalog.ServicesIn(mode) {
		if !s.OneShot {
			out = append(out, s.Name)
		}
	}
	return out
}

// Port ranges of §6.5.
const (
	DefaultHTTPPort   = 80
	DefaultHTTPSPort  = 443
	AutoHTTPPort      = 8080
	AutoHTTPSPort     = 8443
	autoPortTries     = 100
	DevPortsStart     = 15000
	devPortsStep      = 10
	devPortsBaseTries = 100
)

// ChoosePorts picks init's HTTP and HTTPS ports (§6.5). A port given (non
// zero) is used if it is free and is exit 3 otherwise. Without both, 80
// and 443 are used if free; if not, auto allows the first free pair from
// 8080/8443, 8081/8444 and so on, and without it busy defaults are exit 3.
// A port given alone keeps its value while the other is chosen.
func ChoosePorts(h Host, httpPort, httpsPort int, auto bool) (int, int, error) {
	for _, p := range []struct {
		flag string
		port int
	}{{"--http-port", httpPort}, {"--https-port", httpsPort}} {
		if p.port != 0 && !h.PortFree(p.port) {
			return 0, 0, exitcode.Precondition("port %d (%s) is in use", p.port, p.flag)
		}
	}
	if httpPort != 0 && httpsPort != 0 {
		return httpPort, httpsPort, nil
	}
	free := func(p, given int) bool { return given != 0 || h.PortFree(p) }
	pick := func(p, given int) int {
		if given != 0 {
			return given
		}
		return p
	}
	if free(DefaultHTTPPort, httpPort) && free(DefaultHTTPSPort, httpsPort) &&
		pick(DefaultHTTPPort, httpPort) != pick(DefaultHTTPSPort, httpsPort) {
		return pick(DefaultHTTPPort, httpPort), pick(DefaultHTTPSPort, httpsPort), nil
	}
	if !auto {
		return 0, 0, exitcode.Precondition("the default ports %d and %d aren't both free; pass --http-port and --https-port, or --auto-ports to pick free ones",
			pick(DefaultHTTPPort, httpPort), pick(DefaultHTTPSPort, httpsPort))
	}
	for i := range autoPortTries {
		hp, sp := pick(AutoHTTPPort+i, httpPort), pick(AutoHTTPSPort+i, httpsPort)
		if hp != sp && free(hp, httpPort) && free(sp, httpsPort) {
			return hp, sp, nil
		}
	}
	return 0, 0, exitcode.Precondition("no free port pair from %d/%d to %d/%d; pass --http-port and --https-port",
		AutoHTTPPort, AutoHTTPSPort, AutoHTTPPort+autoPortTries-1, AutoHTTPSPort+autoPortTries-1)
}

// ChooseDevPortsBase returns the first base from 15000, in steps of 10,
// whose block of catalog.DevPortSpan ports is free and clear of avoid (the
// stack's HTTP and HTTPS ports).
func ChooseDevPortsBase(h Host, avoid ...int) (int, error) {
next:
	for i := range devPortsBaseTries {
		base := DevPortsStart + i*devPortsStep
		for p := base; p < base+catalog.DevPortSpan; p++ {
			for _, a := range avoid {
				if p == a {
					continue next
				}
			}
			if !h.PortFree(p) {
				continue next
			}
		}
		return base, nil
	}
	return 0, exitcode.Precondition("no free block of %d dev ports from %d", catalog.DevPortSpan, DevPortsStart)
}

// StackNameInUse reports what, if anything, already uses the stack name
// for a stack other than the one in dir: a container or volume of the
// compose project of that name, or a volume labelled for that stack, whose
// stack-dir label isn't dir. "" means the name is free. existing reports
// whether dir's own stack has containers.
func StackNameInUse(ctx context.Context, d *Deps, name, dir string) (user string, existing bool, err error) {
	dir = canonicalDir(dir)
	res, err := docker.RunChecked(ctx, docker.WithTimeout(d.Runner, docker.PsTimeout), docker.Cmd{Argv: []string{
		"docker", "ps", "--all", "--no-trunc",
		"--filter", "label=com.docker.compose.project=" + name,
		"--format", "{{json .}}",
	}})
	if err != nil {
		return "", false, err
	}
	for line := range strings.Lines(string(res.Stdout)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var c struct{ Names, Labels string }
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return "", false, fmt.Errorf("parsing docker ps: %w", err)
		}
		if labelValue(c.Labels, stack.LabelStackDir) != dir {
			return "container " + c.Names, false, nil
		}
		existing = true
	}
	seen := map[string]bool{}
	for _, filter := range []string{"com.docker.compose.project=" + name, stack.LabelStack + "=" + name} {
		vols, err := d.Docker.VolumeList(ctx, filter)
		if err != nil {
			return "", false, err
		}
		for _, v := range vols {
			if !seen[v.Name] && v.Labels[stack.LabelStackDir] != dir {
				return "volume " + v.Name, existing, nil
			}
			seen[v.Name] = true
		}
	}
	return "", existing, nil
}

// labelValue reads one label from docker ps's comma-separated KEY=VALUE
// list. A label value with a comma is cut short; stack-dir values the CLI
// writes are absolute paths, which it compares whole, so a cut one is
// reported as another stack's.
func labelValue(labels, key string) string {
	for kv := range strings.SplitSeq(labels, ",") {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// canonicalDir is dir as stack.Stack.Dir has it, absolute with symlinks
// resolved, also when dir doesn't exist yet.
func canonicalDir(dir string) string {
	dir, _ = filepath.Abs(dir)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	parent, base := filepath.Split(dir)
	if r, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(r, base)
	}
	return dir
}

// InitSummary is init's report (§9.1 step 13).
type InitSummary struct {
	Stack string `json:"stack"`
	Dir   string `json:"dir"`
	// AlreadyInitialized is set when init found a complete stack and did
	// nothing.
	AlreadyInitialized bool         `json:"already_initialized,omitempty"`
	URL                string       `json:"url"`
	Auth0              *StatusAuth0 `json:"auth0"`
	// TokenExpiry is the introspection token's expiry.
	TokenExpiry time.Time `json:"token_expiry,omitzero"`
	NextSteps   []string  `json:"next_steps"`
}

// Summary returns init's summary of the stack in st.
func Summary(st *stack.Stack, cfg *stack.Config, sec *stack.Secrets) *InitSummary {
	a := auth0URLs(cfg)
	s := &InitSummary{
		Stack: cfg.Name,
		Dir:   st.Dir,
		URL:   a.WebOrigin + "/",
		Auth0: a,
		NextSteps: []string{
			"pic-sure data demo    load demo data",
			"pic-sure status       check the stack",
		},
	}
	if sec != nil {
		s.TokenExpiry = sec.IntrospectionTokenExpiry.UTC()
	}
	return s
}

// PeekState reads the state.json of the stack, or partly created stack, in
// dir without opening it, or returns nil when there is none.
func PeekState(dir string) (*stack.State, error) {
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(stack.StateFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s stack.State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, filepath.FromSlash(stack.StateFile)), err)
	}
	return &s, nil
}
