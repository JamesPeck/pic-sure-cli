package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Init's own step IDs, before the plan ops.InitSteps returns. They aren't
// skippable.
const (
	initPreconditions = "preconditions"
	initRelease       = "release"
	initConfig        = "config"
)

func newInitCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "init [DIR]",
		Short: "Create a stack in DIR (default: the current directory) and bring it up",
		Long: `Create a PIC-SURE stack in DIR and bring it up: check the host, fetch the
release, write pic-sure.yaml and the secrets, build the images, install the
TLS certificate, render the compose file, set up and migrate the database,
seed it, install the HPDS key and start the services.

Every config flag sets the pic-sure.yaml key its help names. Secrets are
read from stdin only (--auth0-client-secret-stdin). Ports default to 80
and 443 when they are free; --auto-ports picks a free pair from 8080/8443
otherwise.

A DIR that already has a pic-sure.yaml is resumed: its config is used as it
is, and the steps already done are skipped. On a stack init has finished it
does nothing.`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.initStack,
	}
	for _, f := range stack.Fields {
		switch {
		case f.Flag == "":
		case f.Secret:
			c.Flags().Bool(f.Flag, false, f.Help+" (sets "+f.Key+")")
		default:
			c.Flags().String(f.Flag, "", f.Help+" (sets "+f.Key+")")
		}
	}
	c.Flags().Bool("auto-ports", false, "pick free ports from 8080/8443 when 80 or 443 is busy")
	c.Flags().StringArray("source", nil, "build `COMPONENT=PATH` from a local checkout (repeatable)")
	return c
}

// initRun is one init's state, shared by its steps.
type initRun struct {
	a   *App
	cmd *cobra.Command
	d   *ops.Deps
	dir string
	// resumed is set when DIR already had a pic-sure.yaml.
	resumed bool
	doc     *stack.ConfigDoc
	cfg     *stack.Config

	supplied stack.UserSecrets
	cache    *cache.Cache
	proxy    *netproxy.Proxy
	rel      *release.Release
	prior    *stack.State // state.json before this run, nil if none

	st    *stack.Stack
	lock  *stack.Lock
	sec   *stack.Secrets
	state *stack.State
}

func (a *App) initStack(cmd *cobra.Command, args []string) (err error) {
	ctx := cmd.Context()
	dir, err := a.initDir(args)
	if err != nil {
		return err
	}
	if err := checkInitSkips(a.Global.SkipSteps); err != nil {
		return err
	}
	r := &initRun{a: a, cmd: cmd, dir: dir}
	if r.prior, err = ops.PeekState(dir); err != nil {
		return err
	}
	if r.prior != nil && !r.prior.InitializedAt.IsZero() {
		return a.alreadyInitialized(cmd, dir)
	}
	if err := r.readConfig(); err != nil {
		return err
	}
	if err := r.readSecrets(); err != nil {
		return err
	}
	log.RegisterSecrets(r.cfg.Auth.AdminEmail)

	r.d = a.newDeps()
	defer func() {
		if r.lock != nil {
			_ = r.lock.Unlock()
		}
		if r.st != nil {
			_ = r.st.Close()
		}
	}()
	prep := []steps.Step{
		{ID: initPreconditions, Title: "Check the host", Apply: r.preconditions},
		{ID: initRelease, Title: "Fetch the release", Apply: r.fetchRelease},
		{ID: initConfig, Title: "Write the config and secrets", Apply: r.writeConfig},
	}
	if err := steps.Run(ctx, r.d.Sink, prep, steps.Options{}); err != nil {
		if r.state != nil {
			_ = r.finishOperation(err)
		}
		return err
	}
	if r.state == nil {
		// Another init finished the stack while this one waited for it.
		return a.alreadyInitialized(cmd, dir)
	}

	plan := ops.InitSteps(r.d, r.st, r.cfg, r.sec, r.state, ops.ConvergeOptions{
		Cache:      r.cache,
		CLIVersion: a.Info.Version,
		Compose:    r.compose,
	})
	err = steps.Run(ctx, r.d.Sink, plan, steps.Options{Skip: a.Global.SkipSteps})
	if ferr := r.finishOperation(err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	summary := ops.Summary(r.st, r.cfg, r.sec)
	return a.finish(summary, func(w io.Writer) error { return writeInitSummary(w, summary) })
}

// checkInitSkips refuses a --skip-step that names no step of init's plan,
// before anything is created.
func checkInitSkips(skips []string) error {
	for _, id := range skips {
		if !slices.Contains(ops.InitStepIDs, id) {
			return exitcode.Usage("--skip-step %s: init has no such step; it can skip %s", id, strings.Join(ops.InitStepIDs, ", "))
		}
	}
	return nil
}

func (a *App) alreadyInitialized(cmd *cobra.Command, dir string) error {
	st, err := stack.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := a.gate(cmd, st); err != nil {
		return err
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	sec, err := st.LoadSecrets()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	summary := ops.Summary(st, cfg, sec)
	summary.AlreadyInitialized = true
	return a.finish(summary, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Stack %s in %s is already initialised; use `pic-sure up` or `pic-sure update`.\n", cfg.Name, st.Dir)
		return err
	})
}

// readConfig reads DIR's pic-sure.yaml when there is one, or else makes the
// config from the flags. Problems are exit 2, naming the flag.
func (r *initRun) readConfig() error {
	flags := r.cmd.Flags()
	data, err := os.ReadFile(filepath.Join(r.dir, stack.ConfigFile))
	switch {
	case err == nil:
		r.resumed = true
		if r.doc, err = stack.ParseConfigDoc(data); err != nil {
			return configError(err)
		}
		if r.cfg, err = r.doc.Config(); err != nil {
			return configError(err)
		}
		var ignored []string
		flags.Visit(func(f *pflag.Flag) {
			if isConfigFlag(f.Name) {
				ignored = append(ignored, "--"+f.Name)
			}
		})
		if len(ignored) > 0 {
			r.a.warnStderr("%s already has %s, which init uses as it is; ignoring %s",
				r.dir, stack.ConfigFile, strings.Join(ignored, " "))
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	// name is read-only, so Set refuses it.
	def := stack.DefaultConfig()
	def.Name, _ = flags.GetString("name")
	if r.doc, err = stack.NewConfigDoc(&def); err != nil {
		return err
	}
	for _, f := range stack.Fields {
		if f.Flag == "" || f.Flag == "name" || f.Secret || !flags.Changed(f.Flag) {
			continue
		}
		v, _ := flags.GetString(f.Flag)
		if err := r.setFlag(f, v); err != nil {
			return err
		}
	}
	sources, _ := flags.GetStringArray("source")
	for _, s := range sources {
		comp, path, ok := strings.Cut(s, "=")
		if _, known := catalog.LookupComponent(comp); !ok || !known || path == "" {
			return exitcode.Usage("--source %s: want COMPONENT=PATH, COMPONENT one of %s", s, strings.Join(componentNames(), ", "))
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if err := r.doc.SetValue("components."+comp+".source", abs); err != nil {
			return exitcode.Usage("--source %s: %v", s, err)
		}
	}
	if r.cfg, err = r.doc.Config(); err != nil {
		return flagProblems(err)
	}
	return nil
}

func (r *initRun) setFlag(f stack.Field, v string) error {
	if f.Flag == "hpds-data" {
		mode, name, _ := strings.Cut(v, ":")
		switch {
		case v == string(stack.HPDSLocal):
			return r.doc.SetValue(f.Key, v)
		case mode == string(stack.HPDSShared) && name != "":
			if err := r.doc.SetValue(f.Key, mode); err != nil {
				return err
			}
			return r.doc.SetValue("hpds.shared_name", name)
		}
		return exitcode.Usage("--hpds-data %s: want local or shared:NAME", v)
	}
	if err := r.doc.Set(f.Key, v); err != nil {
		return exitcode.Usage("--%s: %v", f.Flag, err)
	}
	return nil
}

func isConfigFlag(name string) bool {
	if name == "source" || name == "auto-ports" {
		return true
	}
	for _, f := range stack.Fields {
		if f.Flag == name && !f.Secret {
			return true
		}
	}
	return false
}

func componentNames() []string {
	var out []string
	for _, c := range catalog.Components() {
		out = append(out, c.Name)
	}
	return out
}

// flagProblems turns the config's validation problems into a usage error
// that names init's flag for each key that has one.
func flagProblems(err error) error {
	var ce *stack.ConfigError
	if !errors.As(err, &ce) {
		return configError(err)
	}
	var msgs []string
	for _, p := range ce.Problems {
		if f, ok := stack.LookupField(p.Path); ok && f.Flag != "" {
			msgs = append(msgs, fmt.Sprintf("--%s: %s", f.Flag, p.Msg))
		} else {
			msgs = append(msgs, fmt.Sprintf("%s: %s", p.Path, p.Msg))
		}
	}
	return exitcode.Usage("%s", strings.Join(msgs, "; "))
}

// readSecrets reads the --*-stdin secrets, and refuses a missing one the
// config requires. With both flags, stdin holds the Auth0 client secret on
// its first line and the remote root password on its second.
func (r *initRun) readSecrets() error {
	var fields []stack.Field
	for _, f := range stack.Fields {
		if f.Secret && f.Flag != "" {
			if on, _ := r.cmd.Flags().GetBool(f.Flag); on {
				fields = append(fields, f)
			}
		}
	}
	inputs := []io.Reader{r.a.Stdin}
	if len(fields) > 1 {
		data, err := io.ReadAll(io.LimitReader(r.a.Stdin, 1<<20))
		if err != nil {
			return err
		}
		lines := strings.SplitAfter(string(data), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) != len(fields) {
			return exitcode.Usage("--%s and --%s both read stdin: give one secret per line, in that order", fields[0].Flag, fields[1].Flag)
		}
		inputs = inputs[:0]
		for _, l := range lines {
			inputs = append(inputs, strings.NewReader(l))
		}
	}
	for i, f := range fields {
		v, err := stack.ReadUserSecret(inputs[i], "--"+f.Flag)
		if err != nil {
			return err
		}
		switch f.Key {
		case "auth.auth0.client_secret":
			r.supplied.Auth0ClientSecret = v
		case "db.remote.root_password":
			r.supplied.DBRemoteRootPassword = v
		}
	}
	if r.resumed {
		// The stack may have them already; EnsureSecrets checks.
		return nil
	}
	for _, f := range stack.Fields {
		if f.Secret && f.Flag != "" && f.Required(r.cfg) && !slices.ContainsFunc(fields, func(g stack.Field) bool { return g.Key == f.Key }) {
			return exitcode.Usage("--%s is required when %s", f.Flag, f.RequiredWhen.Desc)
		}
	}
	return nil
}

// preconditions is §9.1 step 1: everything is checked before anything is
// written.
func (r *initRun) preconditions(ctx context.Context, sink events.Sink) error {
	opts := ops.DoctorOptions{Host: systemHost{}, Building: r.cfg.Images.Mode == stack.ImagesBuild}
	opts.CacheDir, opts.CacheErr = cache.DefaultRoot()
	report := ops.Doctor(ctx, r.d, opts)
	var failed []string
	for _, c := range report.Checks {
		msg := log.Redact(c.Message)
		switch {
		case c.Status == ops.CheckFail && c.Name != "memory":
			failed = append(failed, c.Name+": "+msg)
		case c.Status != ops.CheckOK:
			// Memory is only a warning: -Xmx is a ceiling, not a reservation.
			sink.Emit(events.Warning{ID: initPreconditions, Text: c.Name + ": " + msg})
		}
	}
	if len(failed) > 0 {
		return exitcode.Precondition("the host isn't ready:\n  %s\nRun `pic-sure doctor` for details", strings.Join(failed, "\n  "))
	}

	user, existing, err := ops.StackNameInUse(ctx, r.d, r.cfg.Name, r.dir)
	if err != nil {
		return err
	}
	if user != "" {
		return exitcode.Precondition("the stack name %s is in use by another stack or compose project (%s); choose another --name", r.cfg.Name, user)
	}
	if r.cfg.DB.Mode == stack.DBRemote {
		if hint := ops.LoopbackHint(r.cfg.DB.Remote.Host); hint != "" {
			sink.Emit(events.Warning{ID: initPreconditions, Text: "the remote database may be unreachable" + hint})
		}
	}

	if r.resumed {
		// A resumed stack keeps its ports. They are checked only before
		// its own containers exist, which would hold them.
		if !existing {
			for _, p := range []int{r.cfg.Network.HTTPPort, r.cfg.Network.HTTPSPort} {
				if !(systemHost{}).PortFree(p) {
					return exitcode.Precondition("port %d, which %s sets, is in use", p, stack.ConfigFile)
				}
			}
		}
		return nil
	}
	flags := r.cmd.Flags()
	httpPort, err := portFlag(flags, "http-port")
	if err != nil {
		return err
	}
	httpsPort, err := portFlag(flags, "https-port")
	if err != nil {
		return err
	}
	auto, _ := flags.GetBool("auto-ports")
	if httpPort, httpsPort, err = ops.ChoosePorts(systemHost{}, httpPort, httpsPort, auto); err != nil {
		return err
	}
	base, err := ops.ChooseDevPortsBase(systemHost{}, httpPort, httpsPort)
	if err != nil {
		return err
	}
	for key, v := range map[string]int{"network.http_port": httpPort, "network.https_port": httpsPort, "network.dev_ports.base": base} {
		if err := r.doc.SetValue(key, v); err != nil {
			return err
		}
	}
	if r.cfg, err = r.doc.Config(); err != nil {
		return flagProblems(err)
	}
	return nil
}

// portFlag is a port flag's value, 0 when not given.
func portFlag(flags *pflag.FlagSet, name string) (int, error) {
	if !flags.Changed(name) {
		return 0, nil
	}
	v, _ := flags.GetString(name)
	p, err := strconv.Atoi(v)
	if err != nil || p < 1 || p > 65535 {
		return 0, exitcode.Usage("--%s %s: not a port number", name, v)
	}
	return p, nil
}

// fetchRelease is §9.1 step 2: release-control into the host cache, and the
// CLI compatibility gate. A resumed stack keeps the release it recorded.
func (r *initRun) fetchRelease(ctx context.Context, sink events.Sink) error {
	var err error
	if r.proxy, err = netproxy.New(netproxy.Config(r.cfg.Proxy), netproxy.CatalogServices()); err != nil {
		return exitcode.Usage("%w", err)
	}
	r.d.Git = r.d.Git.WithEnv(r.proxy.Env()...)
	root, err := cache.DefaultRoot()
	if err != nil {
		return err
	}
	if r.cache, err = cache.Open(root, cache.Options{Git: r.d.Git, Holder: r.cmd.CommandPath()}); err != nil {
		return err
	}
	opts := release.Options{Repo: r.cfg.Release.Repo, Branch: r.cfg.Release.Branch}
	if r.prior != nil && r.prior.Release.Commit != "" {
		opts.Commit = r.prior.Release.Commit
		if r.prior.Release.Repo != "" {
			opts.Repo = r.prior.Release.Repo
		}
	}
	if r.rel, err = release.Fetch(ctx, r.cache.WithEvents(sink, initRelease), r.d.Git, sink, initRelease, opts); err != nil {
		return err
	}
	return r.rel.Gate(ctx, release.GateOptions{
		CLIVersion: r.a.Info.Version,
		Compat:     r.cfg.Release.CLICompat,
		Updater:    r.a.newSelfUpdater(r.proxy, sink, initRelease),
		Command:    "pic-sure init",
		Sink:       sink,
		Step:       initRelease,
	})
}

// writeConfig is §9.1 step 3: the stack directory, pic-sure.yaml, the
// secrets and state.json, under the stack lock.
func (r *initRun) writeConfig(ctx context.Context, sink events.Sink) error {
	st, err := stack.Create(r.dir)
	if err != nil {
		return err
	}
	r.st = st
	r.a.openRunLog(st)
	if r.lock, err = r.a.lockStack(ctx, r.cmd, st, sink); err != nil {
		return err
	}
	// Under the lock, another init may have finished or started the stack.
	prior, err := ops.PeekState(r.dir)
	if err != nil {
		return err
	}
	if prior != nil && !prior.InitializedAt.IsZero() {
		return nil
	}
	if !r.resumed {
		if _, err := os.Stat(st.Path(stack.ConfigFile)); err == nil {
			return exitcode.Failed("another pic-sure init wrote %s meanwhile; run init again to resume it", st.Path(stack.ConfigFile))
		}
		data, err := r.doc.Bytes()
		if err != nil {
			return err
		}
		if err := st.WriteConfig(data); err != nil {
			return err
		}
	}

	r.sec, err = st.EnsureSecrets(r.d.Rand, stack.EnsureOptions{
		RemoteDB: r.cfg.DB.Mode == stack.DBRemote,
		OpenAuth: r.cfg.Auth.Mode == stack.AuthOpen,
		Supplied: r.supplied,
	})
	if err != nil {
		return err
	}
	// The compose env is computed on every call (compose); this is where
	// a problem with it shows.
	if _, err := render.ComposeEnv(r.cfg, r.sec); err != nil {
		return err
	}

	state, err := st.LoadState()
	if errors.Is(err, fs.ErrNotExist) {
		// Versions now, so a step that saves state before render (TLS)
		// doesn't leave them empty.
		state, err = &stack.State{CLIVersion: r.a.Info.Version, SchemaVersion: stack.ConfigSchema}, nil
	}
	if err != nil {
		return err
	}
	if state.Release.Commit == "" {
		state.Release = stack.Release{Repo: r.rel.Repo, Branch: r.rel.Branch, Commit: r.rel.Commit}
	}
	state.StartOperation("init", r.d.Clock.Now())
	if err := st.SaveState(state); err != nil {
		return err
	}
	r.state = state
	return nil
}

// compose is the stack's Composer for the steps after render. Its env is
// computed on each call from r.sec, which the seed step updates in place,
// so `compose up` hands the gateway the token seed issued.
func (r *initRun) compose() (docker.Composer, error) {
	cfg, sec := r.cfg, r.sec
	c, err := docker.NewCompose(r.d.Runner, r.st.Dir, func() []string {
		// writeConfig checked that it succeeds; only secrets change since.
		env, _ := render.ComposeEnv(cfg, sec)
		return env
	})
	if err != nil {
		return nil, err
	}
	if r.a.output().mode == modeJSON {
		c.Progress = docker.ProgressJSON
	}
	return c, nil
}

// finishOperation records how init ended in state.json, and its end when
// it succeeded. The steps save state.json themselves, so it is re-read.
func (r *initRun) finishOperation(runErr error) error {
	state, err := r.st.LoadState()
	if err != nil {
		return err
	}
	now := r.d.Clock.Now()
	state.FinishOperation(runErr, now)
	if runErr == nil {
		state.InitializedAt = now
	}
	return r.st.SaveState(state)
}

func writeInitSummary(w io.Writer, s *ops.InitSummary) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Stack %s is up: %s\n", s.Stack, s.URL)
	if au := s.Auth0; au != nil {
		if au.Needed {
			b.WriteString("Register these in the Auth0 application:\n")
		} else {
			b.WriteString("Auth0 isn't needed in open mode; to allow login, register these:\n")
		}
		fmt.Fprintf(&b, "  Callback URL: %s\n  Logout URL:   %s\n  Web origin:   %s\n", au.CallbackURL, au.LogoutURL, au.WebOrigin)
	}
	if !s.TokenExpiry.IsZero() {
		fmt.Fprintf(&b, "Introspection token expires %s; pic-sure update renews it.\n", s.TokenExpiry.Format(time.DateOnly))
	}
	b.WriteString("Next steps:\n")
	for _, n := range s.NextSteps {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
