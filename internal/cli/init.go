package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
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

// releaseTitle is the title of init's and update's release step.
const releaseTitle = "Fetch the release"

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
pair from 8080/8443 instead. Ports another stack's pic-sure.yaml sets count
as taken. If a port --auto-ports chose, or a dev port, is taken by the time
the stack starts, init chooses again once.

A DIR that already has a pic-sure.yaml is resumed: its config is used as it
is, a config flag, --source or --set that would change it is an error
(change it with pic-sure config set), and the steps already done are
skipped. On a stack init has finished it only registers the stack in
the cache (see cache prune).`,
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
	return skippable(c)
}

// initRun is one init's state, shared by its steps. initStack sets its
// options from the flags, and the TUI's setup wizard sets them itself
// (initFromTUI); run then reads flags only through readConfig and
// readSecrets.
type initRun struct {
	a   *App
	cmd *cobra.Command
	dir string
	// resumed is set when DIR already had a pic-sure.yaml.
	resumed bool
	// doc and cfg are the config. When cfg is nil, run reads it from DIR's
	// pic-sure.yaml or the flags.
	doc *stack.ConfigDoc
	cfg *stack.Config
	// httpPort and httpsPort are the ports asked for, 0 to choose one.
	httpPort, httpsPort int
	autoPorts           bool
	// host is where ports are checked, the system's when nil.
	host     ops.Host
	supplied stack.UserSecrets
	// fromFlags makes run read the --*-stdin secrets.
	fromFlags bool
	// selfUpdate, ignoreCLIVersion and confirm are the gate's options;
	// installOnly makes its self-update install the new pic-sure without
	// re-running, and gateCommand is the command its messages retry.
	selfUpdate, ignoreCLIVersion bool
	confirm                      func(context.Context, string) (bool, error)
	installOnly                  bool
	gateCommand                  string

	d *ops.Deps
	// sets are the --set values.
	sets  []initSet
	cache *cache.Cache
	proxy *netproxy.Proxy
	rel   *release.Release
	prior *stack.State // state.json before this run, nil if none

	st    *stack.Stack
	lock  *stack.Lock
	sec   *stack.Secrets
	state *stack.State
}

func (a *App) initStack(cmd *cobra.Command, args []string) error {
	dir, err := a.initDir(args)
	if err != nil {
		return err
	}
	r := &initRun{a: a, cmd: cmd, dir: dir, fromFlags: true, gateCommand: "pic-sure init"}
	r.readGateFlags()
	summary, err := r.run(cmd.Context())
	if err != nil {
		return err
	}
	if summary.AlreadyInitialized {
		return a.finish(summary, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Stack %s in %s is already initialised; use `pic-sure up` or `pic-sure update`.\n", summary.Stack, summary.Dir)
			return err
		})
	}
	return a.finish(summary, func(w io.Writer) error { return writeInitSummary(w, summary) })
}

// run creates and converges the stack, and returns its summary.
func (r *initRun) run(ctx context.Context) (_ *ops.InitSummary, err error) {
	a := r.a
	if r.prior, err = ops.PeekState(r.dir); err != nil {
		return nil, err
	}
	if r.prior != nil && !r.prior.InitializedAt.IsZero() {
		if len(a.Global.SkipSteps) > 0 {
			return nil, exitcode.Usage("--skip-step: the stack in %s is already initialised, so init runs no steps; use `pic-sure up` or `pic-sure update`", r.dir)
		}
		return a.alreadyInitialized(r.cmd, r.dir)
	}
	if r.cfg == nil {
		if err := r.readConfig(); err != nil {
			return nil, err
		}
	}
	log.RegisterSecrets(r.cfg.Auth.AdminEmail)
	if err := checkSkipSteps(r.cmd, ops.InitStepIDs(r.cfg), a.Global.SkipSteps); err != nil {
		return nil, err
	}
	if r.fromFlags {
		if err := r.readSecrets(); err != nil {
			return nil, err
		}
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
		{ID: initRelease, Title: releaseTitle, Apply: r.fetchRelease},
		{ID: initConfig, Title: "Write the config and secrets", Apply: r.writeConfig},
	}
	if err := steps.Run(ctx, r.d.Sink, prep, steps.Options{}); err != nil {
		return nil, err
	}
	if r.state == nil {
		// Another init finished the stack while this one waited for it.
		return a.alreadyInitialized(r.cmd, r.dir)
	}
	// claimPorts has registered a new stack; this registers a resumed one.
	if err := registerStack(ctx, r.cache, r.d.Sink, r.st, r.cfg.Name); err != nil {
		_ = r.finishOperation(err)
		return nil, err
	}

	opts := ops.ConvergeOptions{
		Cache:      r.cache,
		CLIVersion: a.Info.Version,
		Compose:    r.compose,
	}
	plan := ops.InitSteps(r.d, r.st, r.cfg, r.sec, r.state, opts)
	err = steps.Run(ctx, r.d.Sink, plan, steps.Options{Skip: a.Global.SkipSteps})
	if port, choose := r.portRetry(err); choose != nil {
		err = r.retryPorts(ctx, port, choose, opts)
	}
	if ferr := r.finishOperation(err); err == nil {
		err = ferr
	}
	if err != nil {
		return nil, err
	}
	return ops.Summary(r.st, r.cfg, r.sec), nil
}

func (a *App) alreadyInitialized(cmd *cobra.Command, dir string) (*ops.InitSummary, error) {
	st, err := stack.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	if err := a.gate(cmd, st); err != nil {
		return nil, err
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		return nil, configError(err)
	}
	sec, err := st.LoadSecrets()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// Register though nothing else runs: the stack may have been moved here.
	sink := a.newDeps().Sink
	if err := registerStackInDefaultCache(cmd, sink, st, cfg.Name); err != nil {
		sink.Emit(events.Warning{Text: "couldn't register the stack in the cache, so a cache prune run elsewhere may remove its images: " + err.Error()})
	}
	summary := ops.Summary(st, cfg, sec)
	summary.AlreadyInitialized = true
	return summary, nil
}

// readConfig reads DIR's pic-sure.yaml when there is one, or else makes the
// config from the flags. Problems are exit 2, naming the flag.
func (r *initRun) readConfig() error {
	flags := r.cmd.Flags()
	var err error
	if r.sets, err = parseSets(flags); err != nil {
		return err
	}
	given, err := r.configFlags()
	if err != nil {
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
		// A conflict's message quotes the stored value.
		log.RegisterSecrets(r.cfg.Auth.AdminEmail)
		return r.checkResumed(data, given)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	for _, s := range r.sets {
		if s.key == "name" {
			return exitcode.Usage("--set %s: use --name", s.arg)
		}
	}
	// name is read-only, so Set refuses it.
	def := stack.DefaultConfig()
	def.Name, _ = flags.GetString("name")
	if r.doc, err = stack.NewConfigDoc(&def); err != nil {
		return err
	}
	if err := applyFlags(r.doc, given); err != nil {
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
	r.httpPort, r.httpsPort, r.autoPorts = httpPort, httpsPort, auto
	if err := r.choosePorts(anyPortFree{}); err != nil {
		return err
	}
	if r.cfg, err = r.doc.Config(); err != nil {
		return r.flagProblems(err)
	}
	return nil
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

// configFlag is one config value given on init's command line: a config
// flag other than --name, a --source or a --set.
type configFlag struct {
	// label names it in a conflict between two of them, and arg, with its
	// value, in a conflict with a resumed stack's pic-sure.yaml.
	label, arg string
	// keys are the keys it sets, and values what config set takes for each.
	keys, values []string
	apply        func(*stack.ConfigDoc) error
}

// configFlags lists the config values given on the command line, in the
// order they apply: the config flags, the --sources, then the --sets.
func (r *initRun) configFlags() ([]configFlag, error) {
	flags := r.cmd.Flags()
	var out []configFlag
	for _, f := range stack.Fields {
		if f.Flag == "" || f.Flag == "name" || f.Secret || !flags.Changed(f.Flag) {
			continue
		}
		v, _ := flags.GetString(f.Flag)
		c := configFlag{label: "--" + f.Flag, arg: "--" + f.Flag + " " + v,
			keys: []string{f.Key}, values: []string{v},
			apply: func(d *stack.ConfigDoc) error { return setFlag(d, f, v) }}
		if mode, name, _ := strings.Cut(v, ":"); f.Flag == "hpds-data" && mode == string(stack.HPDSShared) {
			// The name first: config set refuses shared data without one.
			c.keys, c.values = []string{"hpds.shared_name", f.Key}, []string{name, mode}
		}
		out = append(out, c)
	}
	sources, _ := flags.GetStringArray("source")
	for _, s := range sources {
		comp, path, ok := strings.Cut(s, "=")
		if _, known := catalog.LookupComponent(comp); !ok || !known || path == "" {
			return nil, exitcode.Usage("--source %s: want COMPONENT=PATH, COMPONENT one of %s", s, strings.Join(componentNames(), ", "))
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		key := "components." + comp + ".source"
		out = append(out, configFlag{label: "--source " + s, arg: "--source " + s,
			keys: []string{key}, values: []string{abs},
			apply: func(d *stack.ConfigDoc) error {
				if err := d.SetValue(key, abs); err != nil {
					return exitcode.Usage("--source %s: %v", s, err)
				}
				return nil
			}})
	}
	for _, s := range r.sets {
		out = append(out, configFlag{label: "--set " + s.arg, arg: "--set " + s.arg,
			keys: []string{s.key}, values: []string{s.value},
			apply: func(d *stack.ConfigDoc) error {
				if err := d.Set(s.key, s.value); err != nil {
					return exitcode.Usage("--set %s: %v", s.arg, err)
				}
				return nil
			}})
	}
	return out, nil
}

// applyFlags applies the given config values to doc. Two that set the same
// key must give it the same value.
func applyFlags(doc *stack.ConfigDoc, given []configFlag) error {
	setBy := map[string]string{} // the keys set so far, to the label that set them
	for _, c := range given {
		var before []any
		for _, k := range c.keys {
			v, _ := doc.Raw(k)
			before = append(before, v)
		}
		if err := c.apply(doc); err != nil {
			return err
		}
		for i, k := range c.keys {
			if by, ok := setBy[k]; ok {
				if after, _ := doc.Raw(k); !reflect.DeepEqual(before[i], after) {
					return exitcode.Usage("%s and %s set %s to different values", c.label, by, k)
				}
			}
			setBy[k] = c.label
		}
	}
	return nil
}

// checkResumed refuses a config value given on the command line that
// differs from the pic-sure.yaml (data) a resumed init uses as it is. The
// same value is accepted, so the same command resumes.
func (r *initRun) checkResumed(data []byte, given []configFlag) error {
	file := filepath.Join(r.dir, stack.ConfigFile)
	if name, _ := r.cmd.Flags().GetString("name"); r.cmd.Flags().Changed("name") && name != r.cfg.Name {
		return exitcode.Usage("--name %s: %s already has name %s, and a stack's name can't change", name, file, r.cfg.Name)
	}
	probe, err := stack.ParseConfigDoc(data)
	if err != nil {
		return configError(err)
	}
	if err := applyFlags(probe, given); err != nil {
		return err
	}
	// Apply each alone: together, one change can make the config invalid
	// and hide which value differs.
	for _, c := range given {
		probe, err := stack.ParseConfigDoc(data)
		if err != nil {
			return configError(err)
		}
		if err := c.apply(probe); err != nil {
			return err
		}
		cfg, cerr := probe.Config()
		for i, k := range c.keys {
			was, _ := r.cfg.Get(k)
			if cerr == nil {
				if now, _ := cfg.Get(k); reflect.DeepEqual(was, now) {
					continue
				}
			}
			change := "edit it there first"
			if _, err := os.Stat(filepath.Join(r.dir, stack.CLIDir)); err == nil {
				change = fmt.Sprintf("change it with `pic-sure --stack %s config set %s %s` first", shellQuote(r.dir), k, shellQuote(c.values[i]))
			}
			stored := fmt.Sprint(was)
			if stored == "" {
				stored = `""`
			}
			return exitcode.Usage("%s: %s already has %s %s, and init resumes with it as it is; %s",
				c.arg, file, k, stored, change)
		}
	}
	return nil
}

// shellQuote quotes s for a POSIX shell, if it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=,+@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// anyPortFree is a host whose ports are all free.
type anyPortFree struct{ systemHost }

func (anyPortFree) PortFree(int) bool { return true }

func setFlag(doc *stack.ConfigDoc, f stack.Field, v string) error {
	if f.Flag == "hpds-data" {
		mode, name, _ := strings.Cut(v, ":")
		switch {
		case v == string(stack.HPDSLocal):
			return doc.SetValue(f.Key, v)
		case mode == string(stack.HPDSShared) && name != "":
			if err := doc.SetValue(f.Key, mode); err != nil {
				return err
			}
			return doc.SetValue("hpds.shared_name", name)
		}
		return exitcode.Usage("--hpds-data %s: want local or shared:NAME", v)
	}
	if err := doc.Set(f.Key, v); err != nil {
		return exitcode.Usage("--%s: %v", f.Flag, err)
	}
	return nil
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
		} else if p.Path == "hpds.shared_name" && r.cmd.Flags().Changed("hpds-data") {
			msgs = append(msgs, fmt.Sprintf("--hpds-data: %s", p.Msg))
		} else if f, ok := stack.LookupField(p.Path); ok && f.Flag != "" {
			msgs = append(msgs, fmt.Sprintf("--%s: %s", f.Flag, p.Msg))
		} else {
			msgs = append(msgs, fmt.Sprintf("%s: %s", p.Path, p.Msg))
		}
	}
	return exitcode.Usage("%s", strings.Join(msgs, "; "))
}

// readGateFlags sets the gate's options from the command line. On a
// terminal the gate offers the self-update (D12), unless stdin holds the
// --*-stdin secrets: it can't answer, and the re-exec couldn't read them
// again.
func (r *initRun) readGateFlags() {
	r.selfUpdate, _ = r.cmd.Flags().GetBool("self-update")
	r.ignoreCLIVersion, _ = r.cmd.Flags().GetBool("ignore-cli-version")
	if r.a.canOfferSelfUpdate() && len(r.stdinFields()) == 0 {
		r.confirm = r.a.gateConfirm(events.StepStarted{ID: initRelease, Title: releaseTitle})
	}
}

// stdinFields are the secrets whose --*-stdin flag is given.
func (r *initRun) stdinFields() []stack.Field {
	var fields []stack.Field
	for _, f := range stack.Fields {
		if f.Secret && f.Flag != "" {
			if on, _ := r.cmd.Flags().GetBool(f.Flag); on {
				fields = append(fields, f)
			}
		}
	}
	return fields
}

// readSecrets reads the --*-stdin secrets, and refuses a missing one the
// config requires. With both flags, piped stdin holds the Auth0 client
// secret on its first line and the remote root password on its second; a
// terminal is asked for each in turn (readUserSecret).
func (r *initRun) readSecrets() error {
	fields := r.stdinFields()
	if selfUpdate, _ := r.cmd.Flags().GetBool("self-update"); selfUpdate && len(fields) > 0 {
		return exitcode.Usage("--self-update re-runs init, which can't read --%s's stdin again; run pic-sure self-update first", fields[0].Flag)
	}
	inputs := []io.Reader{r.a.Stdin}
	terminal := r.a.stdinIsTerminal()
	if len(fields) > 1 && !terminal {
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
		var v stack.Secret
		var err error
		if terminal {
			v, err = r.a.readUserSecret(r.cmd.Context(), secretPromptNames[f.Key], "--"+f.Flag)
		} else {
			v, err = stack.ReadUserSecret(inputs[i], "--"+f.Flag)
		}
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

// secretPromptNames name the --*-stdin secrets in readUserSecret's prompt.
var secretPromptNames = map[string]string{
	"auth.auth0.client_secret": "Auth0 client secret",
	"db.remote.root_password":  "remote database root password",
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
	if r.cfg.HPDS.Data == stack.HPDSShared {
		// Render checks it too, but only after the images are built.
		if _, err := ops.SharedDataProfile(ctx, r.d, r.cfg.HPDS.SharedName); err != nil {
			return err
		}
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
	// Only a check that there are ports to choose: claimPorts chooses them.
	reserved, _ := reservedInDefaultCache(r.dir)
	if err := r.choosePorts(r.reservingHost(reserved)); err != nil {
		return err
	}
	if r.cfg, err = r.doc.Config(); err != nil {
		return r.flagProblems(err)
	}
	return nil
}

// choosePorts chooses the ports init wasn't given on h.
func (r *initRun) choosePorts(h ops.Host) error {
	return r.setPorts(h, r.httpPort, r.httpsPort, r.autoPorts)
}

// setPorts chooses the ports (§6.5) on host h and sets them in the config
// document.
func (r *initRun) setPorts(h ops.Host, httpPort, httpsPort int, auto bool) error {
	httpPort, httpsPort, err := ops.ChoosePorts(h, httpPort, httpsPort, auto)
	if err != nil {
		return err
	}
	if err := r.doc.SetValue("network.http_port", httpPort); err != nil {
		return err
	}
	if err := r.doc.SetValue("network.https_port", httpsPort); err != nil {
		return err
	}
	if _, given := r.setValue("network.dev_ports.base"); given {
		return nil
	}
	return r.setDevPortsBase(h, httpPort, httpsPort)
}

// setDevPortsBase chooses dev_ports.base on h, clear of the HTTP and HTTPS
// ports, and sets it in the config document.
func (r *initRun) setDevPortsBase(h ops.Host, httpPort, httpsPort int) error {
	base, err := ops.ChooseDevPortsBase(h, httpPort, httpsPort)
	if err != nil {
		return err
	}
	return r.doc.SetValue("network.dev_ports.base", base)
}

// portFlag is a port flag's value, or else its key's --set, 0 when neither
// is given.
func (r *initRun) portFlag(name, key string) (int, error) {
	flags := r.cmd.Flags()
	if !flags.Changed(name) {
		if _, given := r.setValue(key); !given {
			return 0, nil
		}
		// applyFlags has parsed it into the config.
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
	return r.rel.Gate(ctx, r.gateOptions(sink))
}

// gateOptions are the compatibility gate's options for this run.
func (r *initRun) gateOptions(sink events.Sink) release.GateOptions {
	u := r.a.newSelfUpdater(r.proxy, sink, initRelease)
	var updater release.SelfUpdater = u
	if r.installOnly {
		updater = installOnly{u}
	}
	return release.GateOptions{
		CLIVersion:       r.a.Info.Version,
		Compat:           r.cfg.Release.CLICompat,
		SelfUpdate:       r.selfUpdate,
		IgnoreCLIVersion: r.ignoreCLIVersion,
		Confirm:          r.confirm,
		Updater:          updater,
		Command:          r.gateCommand,
		Sink:             sink,
		Step:             initRelease,
	}
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
		if err := r.claimPorts(ctx, sink, initConfig, r.choosePorts); err != nil {
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

// claimPorts chooses a new stack's ports again with choose, now with those
// of every registered stack reserved and the ports in avoid, then writes
// pic-sure.yaml and registers the stack, all under the cache's port lock,
// so a concurrent init sees these ports reserved (§6.5). Lock waits are
// reported under step, if any.
func (r *initRun) claimPorts(ctx context.Context, sink events.Sink, step string, choose func(ops.Host) error, avoid ...int) error {
	c := r.cache.WithEvents(sink, step)
	lock, err := c.LockPorts(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	reserved, err := ops.ReservedPorts(r.cache, r.st.Dir)
	if err != nil {
		return err
	}
	for _, p := range avoid {
		reserved[p] = true
	}
	if err := choose(r.reservingHost(reserved)); err != nil {
		return err
	}
	if r.cfg, err = r.doc.Config(); err != nil {
		return r.flagProblems(err)
	}
	data, err := r.doc.Bytes()
	if err != nil {
		return err
	}
	if err := r.st.WriteConfig(data); err != nil {
		return err
	}
	return c.RegisterStack(ctx, r.st.Dir, r.cfg.Name)
}

// reservingHost is the host with reserved ports busy too, except the ones
// the user gave, which are used as given.
func (r *initRun) reservingHost(reserved map[int]bool) ops.Host {
	reserved = maps.Clone(reserved)
	delete(reserved, r.httpPort)
	delete(reserved, r.httpsPort)
	return ops.ReservingHost{Host: r.portHost(), Reserved: reserved}
}

// reservedInDefaultCache is ops.ReservedPorts for the stack in dir from
// the default cache.
func reservedInDefaultCache(dir string) (map[int]bool, error) {
	root, err := cache.DefaultRoot()
	if err != nil {
		return nil, err
	}
	c, err := cache.Open(root, cache.Options{})
	if err != nil {
		return nil, err
	}
	return ops.ReservedPorts(c, dir)
}

func (r *initRun) portHost() ops.Host {
	if r.host != nil {
		return r.host
	}
	return systemHost{}
}

// portRetry decides whether init chooses ports again after err: only when
// a container couldn't publish a port this init chose itself, which
// another stack or program has taken since. It returns that port and what
// to choose again: the dev ports block, or with --auto-ports the HTTP and
// HTTPS ports not given (and the block with them). Without --auto-ports
// the only ports to choose are 80 and 443, so there is nothing to retry.
func (r *initRun) portRetry(err error) (int, func(ops.Host) error) {
	port := docker.PortAllocated(err)
	if port == 0 || r.resumed {
		return 0, nil
	}
	net := r.cfg.Network
	if _, given := r.setValue("network.dev_ports.base"); !given && port >= net.DevPorts.Base && port < net.DevPorts.Base+catalog.DevPortSpan {
		return port, func(h ops.Host) error { return r.setDevPortsBase(h, net.HTTPPort, net.HTTPSPort) }
	}
	if r.autoPorts && (port == net.HTTPPort && r.httpPort == 0 || port == net.HTTPSPort && r.httpsPort == 0) {
		return port, r.choosePorts
	}
	return 0, nil
}

// retryPorts chooses again with choose, avoiding busy, and runs init's
// plan again from the render step.
func (r *initRun) retryPorts(ctx context.Context, busy int, choose func(ops.Host) error, opts ops.ConvergeOptions) error {
	r.d.Sink.Emit(events.Warning{Text: fmt.Sprintf(
		"port %d was taken after init chose it; choosing the ports again", busy)})
	if err := r.claimPorts(ctx, r.d.Sink, "", choose, busy); err != nil {
		return err
	}
	plan, skip := r.retryPlan(opts)
	r.d.Compose = nil
	return steps.Run(ctx, r.d.Sink, plan, steps.Options{Skip: skip})
}

// retryPlan is init's plan from the render step, with the --skip-step
// values that name its steps.
func (r *initRun) retryPlan(opts ops.ConvergeOptions) ([]steps.Step, []string) {
	plan := ops.InitSteps(r.d, r.st, r.cfg, r.sec, r.state, opts)
	plan = plan[slices.IndexFunc(plan, func(s steps.Step) bool { return s.ID == ops.RenderStepID }):]
	var skip []string
	for _, id := range r.a.Global.SkipSteps {
		if slices.ContainsFunc(plan, func(s steps.Step) bool { return s.ID == id }) {
			skip = append(skip, id)
		}
	}
	return plan, skip
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
