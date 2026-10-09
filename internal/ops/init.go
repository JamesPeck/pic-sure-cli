package ops

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
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

// InitStepIDs are the IDs of InitSteps for a stack with config cfg, in
// order, so init can check --skip-step before it creates anything.
func InitStepIDs(cfg *stack.Config) []string {
	return append([]string{ResolveStepID}, planStepIDs(cfg, false)...)
}

// InitSteps are §9.1 steps 4 to 12, for init once it has written the
// config, secrets and state: resolve the component commits, then planSteps
// without up's restart step. state is updated in place as the steps record
// into state.json.
func InitSteps(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts ConvergeOptions) []steps.Step {
	images := ImagesStep(d, st, cfg, state, ImagesOptions{Cache: opts.Cache})
	return append([]steps.Step{ResolveStep(d, st, cfg, state, opts.Cache)},
		planSteps(d, st, cfg, sec, state, opts, images, false)...)
}

// planStepIDs are the IDs of planSteps, with the restart step when restart
// is set.
func planStepIDs(cfg *stack.Config, restart bool) []string {
	ids := []string{ImagesStepID}
	if hmrOn(cfg) {
		ids = append(ids, NodeImageStepID)
	}
	ids = append(ids, TLSStepID, TruststoreStepID, RenderStepID, StepDB)
	if cfg.DB.Mode == stack.DBRemote {
		ids = append(ids, StepDBBootstrap)
	}
	ids = append(ids, StepMigrate, StepSeed, HPDSKeyStepID)
	if restart {
		ids = append(ids, RestartStepID)
	}
	if hmrOn(cfg) && HostUser() != "" {
		ids = append(ids, HMRVolumeStepID)
	}
	return append(ids, StartStepID)
}

// planSteps are the steps init, up and update share after resolving the
// components, with images as the image step: the images (and httpd-hmr's
// Node tag), TLS and the truststore, a fresh render, then ConvergeSteps,
// with httpd-hmr's volume step before start. With restart (up and update),
// the TLS, truststore and render steps record the restarts they call for
// and a restart step runs them before start.
func planSteps(d *Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts ConvergeOptions, images steps.Step, restart bool) []steps.Step {
	tls, trust, rend := TLSStep(d, st, cfg), StackTruststoreStep(d, st, cfg, state), RenderStep(d, st, cfg, state, opts)
	var r *upRestarts
	if restart {
		r = &upRestarts{d: d, st: st, cfg: cfg, opts: opts}
		tls, trust, rend = r.restartAfter(tls, httpd), r.restartAfter(trust, psama), r.watchRender(rend)
	}
	list := []steps.Step{images}
	if hmrOn(cfg) {
		list = append(list, NodeImageStep(st, cfg, state))
	}
	list = append(list, tls, trust, rend)
	converge := ConvergeSteps(d, st, cfg, sec, opts)
	last := len(converge) - 1
	list = append(list, converge[:last]...)
	if restart {
		list = append(list, withCompose(d, opts, r.step()))
	}
	if user := HostUser(); hmrOn(cfg) && user != "" {
		list = append(list, HMRVolumeStep(d, st, cfg, user))
	}
	return append(list, converge[last])
}

// ConvergeSteps are §9.1 steps 8 to 12, which init, up and update run on a
// rendered stack: the database (DBSteps: bootstrapped too when it is
// remote), migrations, seed, the HPDS key, then `compose up -d --wait`.
// Each gets d.Compose from opts.Compose if it is nil.
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
	ensure := func() error { return ensureCompose(d, opts) }
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

// ensureCompose sets d.Compose from opts.Compose if it is nil.
func ensureCompose(d *Deps, opts ConvergeOptions) error {
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

// ResolveStep is the build command's resolve step, for init: it records
// the component commits of the release state.json names, which init sets to
// the release it fetched and gated. It is done when every component without
// a local source has a release commit.
func ResolveStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, c *cache.Cache) steps.Step {
	return resolveStep(d, st, cfg, state, c, unresolved)
}

// StackTruststoreStep is TruststoreStep for the psama image state.json
// records when the step runs, so it can follow the image step in one plan.
func StackTruststoreStep(d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State) steps.Step {
	inner := func() steps.Step { return TruststoreStep(d, st, cfg, psamaImage(state)) }
	s := inner()
	return steps.Step{
		ID:    s.ID,
		Title: s.Title,
		Check: func(ctx context.Context) (bool, error) { return inner().Check(ctx) },
		Apply: func(ctx context.Context, sink events.Sink) error { return inner().Apply(ctx, sink) },
	}
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
			files, err := renderStack(ctx, d, st, cfg, fresh, opts.Cache)
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

// renderStack renders the stack from cfg and the commits and tags state
// records, without writing anything. With hpds.data: shared, the data set
// must be published on this host.
func renderStack(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, c *cache.Cache) ([]render.File, error) {
	var sharedProfile string
	if cfg.HPDS.Data == stack.HPDSShared {
		var err error
		if sharedProfile, err = SharedDataProfile(ctx, d, cfg.HPDS.SharedName); err != nil {
			return nil, err
		}
	}
	sources := map[string]string{}
	for _, comp := range []string{catalog.PicSure, catalog.Migrations} {
		if componentSource(cfg, comp) != "" {
			continue
		}
		commit := state.Components[comp].Commit
		if commit == "" {
			return nil, exitcode.Precondition("state.json records no commit of %s; run pic-sure build", comp)
		}
		var err error
		if sources[comp], err = c.SourceDir(comp, commit); err != nil {
			return nil, err
		}
	}
	certs, err := CustomCerts(st, cfg)
	if err != nil {
		return nil, err
	}
	volumeLabels, err := existingVolumeLabels(ctx, d, st, cfg.Name)
	if err != nil {
		return nil, err
	}
	return render.Render(render.Input{
		StackDir:             st.Dir,
		Config:               cfg,
		State:                state,
		Sources:              sources,
		CustomTrust:          len(certs) > 0,
		SharedProfile:        sharedProfile,
		HostUser:             HostUser(),
		ExistingVolumeLabels: volumeLabels,
	})
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
// zero) is used if it is free and is exit 3 otherwise. A port not given is
// 80 or 443 if free; with auto it is taken instead from the first free pair
// from 8080/8443, 8081/8444 and so on, and without it a busy default is
// exit 3.
func ChoosePorts(h Host, httpPort, httpsPort int, auto bool) (int, int, error) {
	for _, p := range []struct {
		flag string
		port int
	}{{"--http-port", httpPort}, {"--https-port", httpsPort}} {
		if p.port != 0 && !h.PortFree(p.port) {
			return 0, 0, exitcode.Precondition("port %d (%s) is in use", p.port, p.flag)
		}
	}
	// pair returns the ports to use given defaults hp and sp, or ok false
	// when one it would choose is busy or they clash.
	pair := func(hp, sp int) (int, int, bool) {
		if httpPort != 0 {
			hp = httpPort
		} else if !h.PortFree(hp) {
			return 0, 0, false
		}
		if httpsPort != 0 {
			sp = httpsPort
		} else if !h.PortFree(sp) {
			return 0, 0, false
		}
		return hp, sp, hp != sp
	}
	if !auto {
		if hp, sp, ok := pair(DefaultHTTPPort, DefaultHTTPSPort); ok {
			return hp, sp, nil
		}
		var busy []string
		for _, p := range []struct{ given, port int }{{httpPort, DefaultHTTPPort}, {httpsPort, DefaultHTTPSPort}} {
			if p.given == 0 && !h.PortFree(p.port) {
				busy = append(busy, busyPort(h, p.port))
			}
		}
		if len(busy) == 0 {
			return 0, 0, exitcode.Usage("port %d is the other port's default; pass both --http-port and --https-port, or --auto-ports", httpPort+httpsPort)
		}
		return 0, 0, exitcode.Precondition("port %s is in use; pass --http-port and --https-port, or --auto-ports to pick free ones",
			strings.Join(busy, " and "))
	}
	for i := range autoPortTries {
		if hp, sp, ok := pair(AutoHTTPPort+i, AutoHTTPSPort+i); ok {
			return hp, sp, nil
		}
	}
	return 0, 0, exitcode.Precondition("no free port pair from %d/%d to %d/%d; pass --http-port and --https-port",
		AutoHTTPPort, AutoHTTPSPort, AutoHTTPPort+autoPortTries-1, AutoHTTPSPort+autoPortTries-1)
}

// busyPort names a port h reports busy, saying so when another stack
// reserves it.
func busyPort(h Host, port int) string {
	if r, ok := h.(ReservingHost); ok && r.Reserved[port] {
		return fmt.Sprintf("%d (another stack's)", port)
	}
	return strconv.Itoa(port)
}

// ReservingHost is a Host whose Reserved ports are busy as well as those
// in use, so ChoosePorts and ChooseDevPortsBase pass over them.
type ReservingHost struct {
	Host
	Reserved map[int]bool
}

// PortFree implements Host.
func (h ReservingHost) PortFree(port int) bool {
	return !h.Reserved[port] && h.Host.PortFree(port)
}

// ReservedPorts are the ports the stacks in c's registry, other than the
// one in dir, set in their pic-sure.yaml: the HTTP and HTTPS ports and the
// dev_ports block. Such a stack may be stopped, or still initialising, so
// its ports may be free now and taken later. A stack whose config can't be
// read, a gone one among them, reserves nothing.
func ReservedPorts(c *cache.Cache, dir string) (map[int]bool, error) {
	registry, err := c.RegisteredStacks()
	if err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)
	reserved := map[int]bool{}
	for _, r := range registry {
		if r.Dir == "" || r.Dir == dir {
			continue
		}
		cfg, err := readConfig(r.Dir)
		if err != nil {
			continue
		}
		reserved[cfg.Network.HTTPPort] = true
		reserved[cfg.Network.HTTPSPort] = true
		for p := cfg.Network.DevPorts.Base; p < cfg.Network.DevPorts.Base+catalog.DevPortSpan; p++ {
			reserved[p] = true
		}
	}
	return reserved, nil
}

func readConfig(dir string) (*stack.Config, error) {
	st, err := stack.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	return st.LoadConfig()
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

// CanonicalDir is dir as stack.Stack.Dir has it, absolute with symlinks
// resolved, also when dir or some of its parents don't exist yet: the
// nearest one that exists is resolved.
func CanonicalDir(dir string) string {
	dir, _ = filepath.Abs(dir)
	rest := ""
	for p := dir; ; p = filepath.Dir(p) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(p) == p {
			return dir
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
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
	if cfg.HPDS.Data == stack.HPDSShared {
		// The loaders refuse a shared set; its dictionary still needs loading.
		s.NextSteps = []string{
			"pic-sure dictionary hydrate    load the shared data set's dictionary",
			"pic-sure status                check the stack",
		}
	}
	if a.DevWebOrigin != "" {
		// httpd-hmr replaces httpd, so nothing serves the HTTPS port.
		s.URL = a.DevWebOrigin + "/"
	}
	if sec != nil {
		s.TokenExpiry = sec.IntrospectionTokenExpiry.UTC()
	}
	return s
}
