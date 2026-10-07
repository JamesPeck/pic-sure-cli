package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
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

Every config flag sets the pic-sure.yaml key its help names, and --set
KEY=VALUE sets any non-secret key, such as --set hpds.java_opts=-Xmx2g.
Secrets are read from stdin only (--auth0-client-secret-stdin,
--db-root-password-stdin; with both, one per line in that order). Ports not
given are 80 and 443, which must be free; --auto-ports takes the first free
pair from 8080/8443 instead.

A DIR that already has a pic-sure.yaml is resumed: its config is used as it
is, a --set that would change it is an error, and the steps already done
are skipped. On a stack init has finished it does nothing.`,
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
	c.Flags().Bool("auto-ports", false, "pick free ports from 8080/8443 instead of 80 and 443")
	c.Flags().Bool("self-update", false, "if the release needs a newer pic-sure, install it and continue (not with a --*-stdin flag)")
	c.Flags().Bool("ignore-cli-version", false, "go on even if the release was validated with another pic-sure version")
	c.Flags().StringArray("source", nil, "build `COMPONENT=PATH` from a local checkout (repeatable)")
	c.Flags().StringArray("set", nil, "set any non-secret config `KEY=VALUE`, as pic-sure config set does (repeatable)")
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

	sets     []initSet
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
	log.RegisterSecrets(r.cfg.Auth.AdminEmail)
	if err := checkInitSkips(r.cfg, a.Global.SkipSteps); err != nil {
		return err
	}
	if err := r.readSecrets(); err != nil {
		return err
	}

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
		return err
	}
	if r.state == nil {
		// Another init finished the stack while this one waited for it.
		return a.alreadyInitialized(cmd, dir)
	}
	if err := r.cache.RegisterStack(ctx, r.st.Dir, r.cfg.Name); err != nil {
		return err
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

// checkInitSkips refuses a --skip-step that names no step of init's plan
// for cfg, before anything is created.
func checkInitSkips(cfg *stack.Config, skips []string) error {
	ids := ops.InitStepIDs(cfg)
	for _, id := range skips {
		if !slices.Contains(ids, id) {
			return exitcode.Usage("--skip-step %s: init has no such step; it can skip %s", id, strings.Join(ids, ", "))
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
	var err error
	if r.sets, err = parseSets(flags); err != nil {
		return err
	}
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
		if err := r.checkResumedSets(data); err != nil {
			return err
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
		return refuseShared(r.cfg)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	// name is read-only, so Set refuses it.
	def := stack.DefaultConfig()
	def.Name, _ = flags.GetString("name")
	if r.doc, err = stack.NewConfigDoc(&def); err != nil {
		return err
	}
	flagKeys := map[string]string{} // the keys flags set, to their flag
	for _, f := range stack.Fields {
		if f.Flag == "" || f.Flag == "name" || f.Secret || !flags.Changed(f.Flag) {
			continue
		}
		v, _ := flags.GetString(f.Flag)
		if err := r.setFlag(f, v); err != nil {
			return err
		}
		flagKeys[f.Key] = "--" + f.Flag
		if f.Flag == "hpds-data" && strings.HasPrefix(v, string(stack.HPDSShared)+":") {
			flagKeys["hpds.shared_name"] = "--" + f.Flag
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
		flagKeys["components."+comp+".source"] = "--source " + s
	}
	if err := r.applySets(flagKeys); err != nil {
		return err
	}
	// Validation refuses clashing ports, but preconditions chooses the
	// ports not given only later, on the host. Until then use what it
	// would choose were every port free.
	httpPort, err := r.portFlag("http-port", "network.http_port")
	if err != nil {
		return err
	}
	httpsPort, err := r.portFlag("https-port", "network.https_port")
	if err != nil {
		return err
	}
	auto, _ := flags.GetBool("auto-ports")
	if err := r.setPorts(anyPortFree{}, httpPort, httpsPort, auto); err != nil {
		return err
	}
	if r.cfg, err = r.doc.Config(); err != nil {
		return r.flagProblems(err)
	}
	return refuseShared(r.cfg)
}

// initSet is one --set KEY=VALUE.
type initSet struct{ arg, key, value string }

// parseSets splits the --set flags and refuses the secrets, which init
// reads from stdin. ConfigDoc.Set, config set's parser, checks the rest.
func parseSets(flags *pflag.FlagSet) ([]initSet, error) {
	args, _ := flags.GetStringArray("set")
	var sets []initSet
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok || key == "" {
			return nil, exitcode.Usage("--set %s: want KEY=VALUE", arg)
		}
		f, _ := stack.LookupField(key)
		switch {
		case f.Secret && f.Flag != "":
			return nil, exitcode.Usage("--set %s: %s is a secret; give it on stdin with --%s", key, key, f.Flag)
		case f.Secret:
			return nil, exitcode.Usage("--set %s: %s is a secret, kept in .pic-sure/secrets.yaml; init can't set it", key, key)
		}
		sets = append(sets, initSet{arg: arg, key: key, value: value})
	}
	return sets, nil
}

// applySets sets each --set in a new config, after the flags. A key a flag
// or an earlier --set also sets must get the same value from each.
func (r *initRun) applySets(flagKeys map[string]string) error {
	for _, s := range r.sets {
		if s.key == "name" {
			return exitcode.Usage("--set %s: use --name", s.arg)
		}
		before, _ := r.doc.Raw(s.key)
		if err := r.doc.Set(s.key, s.value); err != nil {
			return exitcode.Usage("--set %s: %v", s.arg, err)
		}
		if flag, ok := flagKeys[s.key]; ok {
			if after, _ := r.doc.Raw(s.key); !reflect.DeepEqual(before, after) {
				return exitcode.Usage("--set %s and %s set %s to different values", s.arg, flag, s.key)
			}
		}
		flagKeys[s.key] = "--set " + s.arg
	}
	return nil
}

// checkResumedSets refuses a --set that would change the config a resumed
// init uses as it is.
func (r *initRun) checkResumedSets(data []byte) error {
	for _, s := range r.sets {
		probe, err := stack.ParseConfigDoc(data)
		if err != nil {
			return configError(err)
		}
		if err := probe.Set(s.key, s.value); err != nil {
			return exitcode.Usage("--set %s: %v", s.arg, err)
		}
		cfg, err := probe.Config()
		if err != nil {
			return exitcode.Usage("--set %s: %v", s.arg, err)
		}
		was, _ := r.cfg.Get(s.key)
		if now, _ := cfg.Get(s.key); !reflect.DeepEqual(was, now) {
			return exitcode.Usage("--set %s: %s already has %s %v, and init resumes with it as it is; change it there first",
				s.arg, filepath.Join(r.dir, stack.ConfigFile), s.key, was)
		}
	}
	return nil
}

// setValue is the last --set value for key.
func (r *initRun) setValue(key string) (value string, ok bool) {
	for _, s := range r.sets {
		if s.key == key {
			value, ok = s.value, true
		}
	}
	return value, ok
}

// refuseShared refuses shared HPDS data, which init can't set up until
// ticket 051: render needs the data set's recorded HPDS profile.
func refuseShared(cfg *stack.Config) error {
	if cfg.HPDS.Data == stack.HPDSShared {
		return exitcode.Usage("init doesn't support shared HPDS data yet; use --hpds-data local")
	}
	return nil
}

// anyPortFree is a host whose ports are all free.
type anyPortFree struct{ systemHost }

func (anyPortFree) PortFree(int) bool { return true }

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
// that names the --set or init's flag for each key that has one.
func (r *initRun) flagProblems(err error) error {
	var ce *stack.ConfigError
	if !errors.As(err, &ce) {
		return configError(err)
	}
	var msgs []string
	for _, p := range ce.Problems {
		if v, ok := r.setValue(p.Path); ok {
			msgs = append(msgs, fmt.Sprintf("--set %s=%s: %s", p.Path, v, p.Msg))
		} else if f, ok := stack.LookupField(p.Path); ok && f.Flag != "" {
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
	if selfUpdate, _ := r.cmd.Flags().GetBool("self-update"); selfUpdate && len(fields) > 0 {
		return exitcode.Usage("--self-update re-runs init, which can't read --%s's stdin again; run pic-sure self-update first", fields[0].Flag)
	}
	for i, f := range fields {
		v, err := stack.ReadUserSecret(inputs[i], "--"+f.Flag)
		if err != nil {
			return err
		}
		switch f.Key {
		case "auth.auth0.client_secret":
			// PSAMA signs the introspection token with it, and refuses a
			// shorter key.
			if len(v) < jwt.MinSecretLen {
				return exitcode.Usage("--%s: the client secret is %d bytes; PSAMA needs at least %d", f.Flag, len(v), jwt.MinSecretLen)
			}
			r.supplied.Auth0ClientSecret = v
		case "db.remote.root_password":
			r.supplied.DBRemoteRootPassword = v
		}
	}
	if r.resumed {
		// The stack may have them already; writeConfig checks.
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
	opts := ops.DoctorOptions{Host: systemHost{}, Config: r.cfg, Building: r.cfg.Images.Mode == stack.ImagesBuild}
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

	user, published, err := ops.StackNameInUse(ctx, r.d, r.cfg.Name, r.dir)
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
		// A resumed stack keeps its ports, which its own containers may
		// already hold.
		for _, p := range []int{r.cfg.Network.HTTPPort, r.cfg.Network.HTTPSPort} {
			if !published[p] && !(systemHost{}).PortFree(p) {
				return exitcode.Precondition("port %d, which %s sets, is in use", p, stack.ConfigFile)
			}
		}
		return nil
	}
	flags := r.cmd.Flags()
	httpPort, err := r.portFlag("http-port", "network.http_port")
	if err != nil {
		return err
	}
	httpsPort, err := r.portFlag("https-port", "network.https_port")
	if err != nil {
		return err
	}
	auto, _ := flags.GetBool("auto-ports")
	if err := r.setPorts(systemHost{}, httpPort, httpsPort, auto); err != nil {
		return err
	}
	if r.cfg, err = r.doc.Config(); err != nil {
		return r.flagProblems(err)
	}
	return nil
}

// setPorts chooses the ports (§6.5) on host h and sets them in the config
// document.
func (r *initRun) setPorts(h ops.Host, httpPort, httpsPort int, auto bool) error {
	httpPort, httpsPort, err := ops.ChoosePorts(h, httpPort, httpsPort, auto)
	if err != nil {
		return err
	}
	ports := map[string]int{"network.http_port": httpPort, "network.https_port": httpsPort}
	if _, given := r.setValue("network.dev_ports.base"); !given {
		base, err := ops.ChooseDevPortsBase(h, httpPort, httpsPort)
		if err != nil {
			return err
		}
		ports["network.dev_ports.base"] = base
	}
	for key, v := range ports {
		if err := r.doc.SetValue(key, v); err != nil {
			return err
		}
	}
	return nil
}

// portFlag is a port flag's value, or else its key's --set, 0 when neither
// is given.
func (r *initRun) portFlag(name, key string) (int, error) {
	flags := r.cmd.Flags()
	if !flags.Changed(name) {
		if _, given := r.setValue(key); !given {
			return 0, nil
		}
		// applySets has parsed it into the config.
		v, _ := r.doc.Raw(key)
		if p, _ := v.(int); p >= 1 && p <= 65535 {
			return p, nil
		}
		return 0, exitcode.Usage("--set %s=%v: not a port number", key, v)
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
	if r.rel, err = release.Fetch(ctx, r.cache.WithEvents(sink, initRelease), r.d.Git, sink, initRelease, r.releaseOptions()); err != nil {
		return err
	}
	selfUpdate, _ := r.cmd.Flags().GetBool("self-update")
	ignore, _ := r.cmd.Flags().GetBool("ignore-cli-version")
	return r.rel.Gate(ctx, release.GateOptions{
		CLIVersion:       r.a.Info.Version,
		Compat:           r.cfg.Release.CLICompat,
		SelfUpdate:       selfUpdate,
		IgnoreCLIVersion: ignore,
		Updater:          r.a.newSelfUpdater(r.proxy, sink, initRelease),
		Command:          "pic-sure init",
		Sink:             sink,
		Step:             initRelease,
	})
}

// releaseOptions is the release to fetch: the one a resumed stack
// recorded, else the head of release.branch.
func (r *initRun) releaseOptions() release.Options {
	opts := release.Options{Repo: r.cfg.Release.Repo, Branch: r.cfg.Release.Branch}
	if r.prior != nil && r.prior.Release.Commit != "" {
		opts.Commit = r.prior.Release.Commit
		if r.prior.Release.Repo != "" {
			opts.Repo = r.prior.Release.Repo
		}
	}
	return opts
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
	switch {
	case r.sec.Auth0ClientSecret == "":
		return exitcode.Usage("--auth0-client-secret-stdin is required: the stack has no Auth0 client secret, and auth.mode is %s", r.cfg.Auth.Mode)
	case len(r.sec.Auth0ClientSecret) < jwt.MinSecretLen:
		return exitcode.Precondition("the stack's Auth0 client secret is shorter than the %d bytes PSAMA needs; correct it in %s",
			jwt.MinSecretLen, st.Path(stack.SecretsFile))
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
		// ComposeEnv fails only on the proxy config, which fetchRelease
		// has checked.
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
	if au := s.Auth0; au != nil && au.Needed {
		b.WriteString("Register these in the Auth0 application:\n")
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
