package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

func newUpCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Converge an existing stack to running",
		Long: `Bring the stack to running: build any missing images, install the TLS
certificate and truststore if their volumes need them, re-render the compose
file, then start and migrate the database, seed it, install the HPDS key and
start the services. Each step is skipped when it is already done, so on a
running, current stack up only verifies. After a reset it re-migrates,
re-seeds and re-keys HPDS.

Running services whose certificate, truststore or rendered files changed
are restarted before the services are started and waited for.`,
		Args: cobra.NoArgs,
		RunE: a.up,
	}
}

func (a *App) up(cmd *cobra.Command, _ []string) (err error) {
	ctx := cmd.Context()
	st, err := a.openStack(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	log.RegisterSecrets(cfg.Auth.AdminEmail)
	if err := checkUpSkips(cfg, a.Global.SkipSteps); err != nil {
		return err
	}
	d := a.newDeps()
	lock, err := a.lockStack(ctx, cmd, st, d.Sink)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	// Under the lock: the config may have changed while we waited.
	if cfg, err = st.LoadConfig(); err != nil {
		return configError(err)
	}
	if err := cfg.CheckFiles(st.Dir); err != nil {
		return configError(err)
	}
	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", st.Dir, st.Dir)
	}
	if err != nil {
		return err
	}
	sec, err := upSecrets(d, st, cfg)
	if err != nil {
		return err
	}
	if err := checkUpPorts(cmd, d, st, cfg); err != nil {
		return err
	}
	proxy, err := netproxy.New(netproxy.Config(cfg.Proxy), netproxy.CatalogServices())
	if err != nil {
		return exitcode.Usage("%w", err)
	}
	d.Git = d.Git.WithEnv(proxy.Env()...)
	root, err := cache.DefaultRoot()
	if err != nil {
		return err
	}
	c, err := cache.Open(root, cache.Options{Git: d.Git, Holder: cmd.CommandPath()})
	if err != nil {
		return err
	}
	if err := registerStack(ctx, c, d.Sink, st, cfg.Name); err != nil {
		return err
	}

	state.StartOperation("up", d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	plan := ops.UpSteps(d, st, cfg, sec, state, ops.ConvergeOptions{
		Cache:      c,
		CLIVersion: a.Info.Version,
		Compose:    a.upCompose(d, st, cfg, sec),
	})
	err = steps.Run(ctx, d.Sink, plan, steps.Options{Skip: a.Global.SkipSteps})
	if ferr := finishUp(d, st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	summary := ops.Summary(st, cfg, sec)
	summary.NextSteps = []string{}
	return a.finish(summary, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Stack %s is up: %s\n", summary.Stack, summary.URL)
		return err
	})
}

// checkUpSkips refuses a --skip-step that names no step of up's plan for
// cfg, before the lock is taken.
func checkUpSkips(cfg *stack.Config, skips []string) error {
	ids := ops.UpStepIDs(cfg)
	for _, id := range skips {
		if !slices.Contains(ids, id) {
			return exitcode.Usage("--skip-step %s: up has no such step; it can skip %s", id, strings.Join(ids, ", "))
		}
	}
	return nil
}

// upSecrets loads secrets.yaml, generating any generated secret it lacks
// but never replacing one (§9.11). A client secret generated for open mode
// is refused once the stack has left open mode: only `secrets rotate`
// takes the real one.
func upSecrets(d *ops.Deps, st *stack.Stack, cfg *stack.Config) (*stack.Secrets, error) {
	sec, err := st.LoadSecrets()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, exitcode.Precondition("the stack in %s has no %s; run `pic-sure init %s` to finish it", st.Dir, stack.SecretsFile, st.Dir)
	}
	if err != nil {
		return nil, err
	}
	if err := refuseClientSecret(cfg, sec); err != nil {
		return nil, err
	}
	sec, err = st.EnsureSecrets(d.Rand, stack.EnsureOptions{
		RemoteDB: cfg.DB.Mode == stack.DBRemote,
		OpenAuth: cfg.Auth.Mode == stack.AuthOpen,
	})
	if err != nil {
		return nil, err
	}
	if len(sec.Auth0ClientSecret) < jwt.MinSecretLen {
		return nil, shortClientSecret()
	}
	return sec, nil
}

func shortClientSecret() error {
	return exitcode.Precondition("the stack's Auth0 client secret is shorter than the %d bytes PSAMA needs; %s",
		jwt.MinSecretLen, rotateClientSecret)
}

const rotateClientSecret = "pipe the Auth0 application's client secret to `pic-sure secrets rotate auth0-client-secret`"

// refuseClientSecret refuses, outside open mode, a client secret that is
// missing or was generated for open mode.
func refuseClientSecret(cfg *stack.Config, sec *stack.Secrets) error {
	if cfg.Auth.Mode == stack.AuthOpen {
		return nil
	}
	switch {
	case sec.Auth0ClientSecretGenerated:
		return exitcode.Precondition("auth.mode is %s, but the stack's Auth0 client secret is a random one made for open mode; %s", cfg.Auth.Mode, rotateClientSecret)
	case sec.Auth0ClientSecret == "":
		return exitcode.Precondition("auth.mode is %s, but the stack has no Auth0 client secret; %s", cfg.Auth.Mode, rotateClientSecret)
	}
	return nil
}

// checkUpPorts makes sure the stack's HTTP and HTTPS ports are free, or
// published by its own containers, so a busy port is exit 3 rather than a
// failed `compose up` after the database work.
func checkUpPorts(cmd *cobra.Command, d *ops.Deps, st *stack.Stack, cfg *stack.Config) error {
	user, published, err := ops.StackNameInUse(cmd.Context(), d, cfg.Name, st.Dir)
	if err != nil {
		return err
	}
	if user != "" {
		return exitcode.Precondition("the stack name %s is in use by another stack or compose project (%s)", cfg.Name, user)
	}
	for _, p := range []int{cfg.Network.HTTPPort, cfg.Network.HTTPSPort} {
		if !published[p] && !(systemHost{}).PortFree(p) {
			return exitcode.Precondition("port %d, which %s sets, is in use", p, stack.ConfigFile)
		}
	}
	return nil
}

// upCompose is the stack's Composer for the steps after render. Like
// init's, its env is computed on each call from sec, which the seed step
// updates in place, so `compose up` hands the gateway the token seed
// issued.
func (a *App) upCompose(d *ops.Deps, st *stack.Stack, cfg *stack.Config, sec *stack.Secrets) func() (docker.Composer, error) {
	return func() (docker.Composer, error) {
		c, err := docker.NewCompose(d.Runner, st.Dir, func() []string {
			// ComposeEnv fails only on the proxy config, which up has
			// checked.
			env, _ := render.ComposeEnv(cfg, sec)
			return env
		})
		if err != nil {
			return nil, err
		}
		if a.output().mode == modeJSON {
			c.Progress = docker.ProgressJSON
		}
		return c, nil
	}
}

// finishUp records how up ended in state.json. The steps save state.json
// themselves, so it is re-read.
func finishUp(d *ops.Deps, st *stack.Stack, runErr error) error {
	state, err := st.LoadState()
	if err != nil {
		return err
	}
	state.FinishOperation(runErr, d.Clock.Now())
	return st.SaveState(state)
}
