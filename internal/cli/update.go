package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/jwt"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// update's own steps before the plan runs, which --skip-step can't name.
const (
	updateRelease = "release"
	updatePlan    = "plan"
)

func newUpdateCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Update the stack to the current release: config, images, migrations",
		Long: `Move the stack to the head of release.branch, or to --release-commit: work
out a plan (config migrations, component commits, images, database
migrations, the introspection token, services to restart), then back up
and migrate pic-sure.yaml, build or pull the images, re-render, migrate,
seed (renewing the token when it expires within 30 days), and start the
stack, restarting only the services that need it.

--dry-run prints the plan and changes nothing in the stack, though it may
start the stack's database to compare its migrations. --no-build keeps the
components and images the stack runs and doesn't move its release.`,
		Args: cobra.NoArgs,
		RunE: a.update,
	}
	f := c.Flags()
	f.Bool("dry-run", false, "print the plan and change nothing")
	f.String("release-commit", "", "use this release-control `SHA` instead of the branch head")
	f.Bool("no-build", false, "skip building and pulling images")
	f.Bool("self-update", false, "replace this binary if the release names a newer CLI")
	f.Bool("ignore-cli-version", false, "skip the CLI compatibility gate")
	return skippable(c)
}

// updateRun is one update: its flags, then what it reads as it goes.
type updateRun struct {
	a   *App
	cmd *cobra.Command

	dryRun, noBuild, selfUpdate, ignoreCLIVersion bool
	releaseCommit                                 string

	d     *ops.Deps
	st    *stack.Stack
	doc   *stack.ConfigDoc // pic-sure.yaml as read
	cfg   *stack.Config    // migrated in memory
	sec   *stack.Secrets
	state *stack.State
	cache *cache.Cache
	proxy *netproxy.Proxy
	rel   *release.Release
	comps map[string]stack.Component // rel's component commits
	plan  *ops.UpdatePlan
}

func (a *App) update(cmd *cobra.Command, _ []string) (err error) {
	r := &updateRun{a: a, cmd: cmd}
	f := cmd.Flags()
	r.dryRun, _ = f.GetBool("dry-run")
	r.noBuild, _ = f.GetBool("no-build")
	r.selfUpdate, _ = f.GetBool("self-update")
	r.ignoreCLIVersion, _ = f.GetBool("ignore-cli-version")
	r.releaseCommit, _ = f.GetString("release-commit")
	if r.noBuild && r.releaseCommit != "" {
		return exitcode.Usage("--release-commit can't be used with --no-build, which keeps the stack's release")
	}
	if r.dryRun && len(a.Global.SkipSteps) > 0 {
		return exitcode.Usage("--skip-step can't be used with --dry-run, which runs no steps")
	}

	ctx := cmd.Context()
	open := a.openStackToCheckSkips
	if r.dryRun {
		// A run log is a write to the stack, and its retention removes old
		// ones.
		open = a.openStackUnlogged
	}
	if r.st, err = open(cmd); err != nil {
		return err
	}
	defer func() { _ = r.st.Close() }()
	if err := r.readConfig(); err != nil {
		return err
	}
	if err := a.checkStackSkipSteps(cmd, r.st, ops.UpdateStepIDs(r.cfg)); err != nil {
		return err
	}
	r.d = a.newDeps()
	lock, err := a.lockStack(ctx, cmd, r.st, r.d.Sink)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	if err := r.preconditions(); err != nil {
		return err
	}

	prep := []steps.Step{
		{ID: updateRelease, Title: releaseTitle, Apply: r.fetchRelease},
		{ID: updatePlan, Title: "Plan the update", Apply: r.makePlan},
	}
	if err := steps.Run(ctx, r.d.Sink, prep, steps.Options{}); err != nil {
		return err
	}
	if r.dryRun {
		r.plan.DryRun = true
		return a.finish(r.plan, func(w io.Writer) error { return writeUpdatePlan(w, r.plan) })
	}

	// Past the gate, so a refused update hasn't written secrets.yaml.
	if r.sec, err = upSecrets(r.d, r.st, r.cfg); err != nil {
		return err
	}
	r.d.Compose = nil // its env was computed from the secrets as read
	r.state.StartOperation("update", r.d.Clock.Now())
	if err := r.st.SaveState(r.state); err != nil {
		return err
	}
	skips := a.Global.SkipSteps
	if r.noBuild {
		skips = append(slices.Clone(skips), ops.ImagesStepID)
		// The node tag httpd-hmr runs is an image too; keep it.
		if slices.Contains(ops.UpdateStepIDs(r.cfg), ops.NodeImageStepID) {
			skips = append(skips, ops.NodeImageStepID)
		}
	}
	plan := ops.UpdateSteps(r.d, r.st, r.plan, r.sec, r.state, r.options())
	err = steps.Run(ctx, r.d.Sink, plan, steps.Options{Skip: skips})
	if ferr := finishUp(r.d, r.st, err); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	summary := ops.Summary(r.st, r.cfg, r.sec)
	return a.finish(r.plan, func(w io.Writer) error {
		if !r.plan.Changes() {
			_, err := fmt.Fprintf(w, "Stack %s was already up to date: %s\n", summary.Stack, summary.URL)
			return err
		}
		if _, err := fmt.Fprintf(w, "Stack %s is updated and up: %s\n", summary.Stack, summary.URL); err != nil {
			return err
		}
		return writeUpdatePlan(w, r.plan)
	})
}

// readConfig reads pic-sure.yaml and migrates it in memory: until the
// config step, the file may be an older schema.
func (r *updateRun) readConfig() error {
	data, err := r.st.ReadFile(stack.ConfigFile)
	if err != nil {
		return err
	}
	doc, err := stack.ParseConfigDoc(data)
	if err != nil {
		return configError(err)
	}
	migrated, err := stack.ParseConfigDoc(data)
	if err != nil {
		return configError(err)
	}
	if _, err := r.a.configMigrations().Migrate(migrated); err != nil {
		return configError(err)
	}
	cfg, err := migrated.Config()
	if err != nil {
		return configError(err)
	}
	r.doc, r.cfg = doc, cfg
	log.RegisterSecrets(cfg.Auth.AdminEmail)
	return nil
}

// preconditions are up's, under the stack lock: the config (re-read, since
// it may have changed while we waited), an initialised stack, its secrets
// and its ports. secrets.yaml is only read: update fills in missing
// secrets after the gate.
func (r *updateRun) preconditions() error {
	if err := r.readConfig(); err != nil {
		return err
	}
	if err := r.cfg.CheckFiles(r.st.Dir); err != nil {
		return configError(err)
	}
	state, err := r.st.LoadState()
	if errors.Is(err, fs.ErrNotExist) || err == nil && state.InitializedAt.IsZero() {
		return exitcode.Precondition("the stack in %s isn't initialised; run `pic-sure init %s` to finish it", r.st.Dir, r.st.Dir)
	}
	if err != nil {
		return err
	}
	r.state = state
	sec, err := r.st.LoadSecrets()
	if errors.Is(err, fs.ErrNotExist) {
		return exitcode.Precondition("the stack in %s has no %s; run `pic-sure init %s` to finish it", r.st.Dir, stack.SecretsFile, r.st.Dir)
	}
	if err != nil {
		return err
	}
	if err := checkSecrets(r.cfg, sec); err != nil {
		return err
	}
	r.sec = sec
	return checkUpPorts(r.cmd, r.d, r.st, r.cfg)
}

// checkSecrets refuses, before the gate and for a dry run too, the
// secrets upSecrets would refuse after it.
func checkSecrets(cfg *stack.Config, sec *stack.Secrets) error {
	if err := refuseClientSecret(cfg, sec); err != nil {
		return err
	}
	if cfg.DB.Mode == stack.DBRemote && sec.DBRemoteRootPassword == "" {
		return exitcode.Precondition("the stack uses a remote database, but no root password was given for it")
	}
	if s := sec.Auth0ClientSecret; s != "" && len(s) < jwt.MinSecretLen {
		return shortClientSecret()
	}
	return nil
}

// fetchRelease is §9.3 step 1: release-control into the host cache, and
// the CLI compatibility gate, then the release's component commits. The
// gate runs before anything but the stack lock is held, since a
// self-update re-executes pic-sure. With --no-build it reads the release
// the stack runs, which update doesn't move.
func (r *updateRun) fetchRelease(ctx context.Context, sink events.Sink) error {
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
	opts := release.Options{Repo: r.cfg.Release.Repo, Branch: r.cfg.Release.Branch, Commit: r.releaseCommit}
	if rec := r.state.Release; r.noBuild && rec.Commit != "" {
		opts.Commit = rec.Commit
		if rec.Repo != "" {
			opts.Repo = rec.Repo
		}
	}
	if r.rel, err = release.Fetch(ctx, r.cache.WithEvents(sink, updateRelease), r.d.Git, sink, updateRelease, opts); err != nil {
		return err
	}
	err = r.rel.Gate(ctx, r.gateOptions(sink))
	if err != nil || r.noBuild {
		return err
	}
	r.comps, err = r.rel.ResolveComponents(ctx, r.cache.WithEvents(sink, updateRelease), sink, updateRelease, r.cfg.Components)
	return err
}

// gateOptions are the compatibility gate's options for this run. On a
// terminal, or in the TUI's dialog, the gate offers the self-update (D12).
func (r *updateRun) gateOptions(sink events.Sink) release.GateOptions {
	u := r.a.newSelfUpdater(r.proxy, sink, updateRelease)
	opts := release.GateOptions{
		CLIVersion:       r.a.Info.Version,
		Compat:           r.cfg.Release.CLICompat,
		SelfUpdate:       r.selfUpdate,
		IgnoreCLIVersion: r.ignoreCLIVersion,
		Updater:          u,
		Command:          "pic-sure update",
		Sink:             sink,
		Step:             updateRelease,
	}
	switch {
	case r.a.tuiConfirm != nil:
		opts.Confirm = r.a.tuiConfirm
		opts.Updater = installOnly{u}
		opts.Command = "pic-sure"
	case r.a.canOfferSelfUpdate():
		opts.Confirm = r.a.gateConfirm(events.StepStarted{ID: updateRelease, Title: releaseTitle})
	}
	return opts
}

// makePlan is §9.3 step 2. The stack is registered in the cache first, so
// a prune elsewhere keeps the trees the plan resolves.
func (r *updateRun) makePlan(ctx context.Context, _ events.Sink) error {
	if err := registerStack(ctx, r.cache, r.d.Sink, r.st, r.cfg.Name); err != nil {
		return err
	}
	var err error
	r.plan, err = ops.PlanUpdate(ctx, r.d, r.st, r.doc, r.cfg, r.sec, r.state, r.options())
	return err
}

func (r *updateRun) options() ops.UpdateOptions {
	return ops.UpdateOptions{
		ConvergeOptions: ops.ConvergeOptions{
			Cache:      r.cache,
			CLIVersion: r.a.Info.Version,
			Compose:    r.a.upCompose(r.d, r.st, r.cfg, r.sec),
		},
		Release:    stack.Release{Repo: r.rel.Repo, Branch: r.rel.Branch, Commit: r.rel.Commit},
		Components: r.comps,
		Migrations: r.a.configMigrations(),
		NoBuild:    r.noBuild,
		StartDB:    true,
	}
}

// writeUpdatePlan prints the plan for people.
func writeUpdatePlan(w io.Writer, p *ops.UpdatePlan) error {
	var b strings.Builder
	if p.DryRun {
		fmt.Fprintf(&b, "Update plan for stack %s (dry run: nothing in the stack was changed)\n", p.Stack)
	}
	if len(p.Config.Migrations) == 0 {
		fmt.Fprintf(&b, "  config:      schema %d, nothing to migrate\n", p.Config.From)
	} else {
		fmt.Fprintf(&b, "  config:      schema %d → %d, backed up first\n", p.Config.From, p.Config.To)
		for _, m := range p.Config.Migrations {
			fmt.Fprintf(&b, "                %s\n", m)
		}
	}
	if p.Release.From == p.Release.To {
		fmt.Fprintf(&b, "  release:     %s (unchanged)\n", shortSHA(p.Release.To))
	} else {
		fmt.Fprintf(&b, "  release:     %s → %s\n", shortSHA(p.Release.From), shortSHA(p.Release.To))
	}
	b.WriteString("  components:\n")
	for _, c := range p.Components {
		to := describeCommit(c.ToRef, c.ToCommit, c.Source)
		if c.Changed {
			fmt.Fprintf(&b, "    %-15s %s → %s\n", c.Name, describeCommit(c.FromRef, c.FromCommit, ""), to)
		} else {
			fmt.Fprintf(&b, "    %-15s %s (unchanged)\n", c.Name, to)
		}
	}
	byAction := map[string][]string{}
	for _, i := range p.Images {
		byAction[i.Action] = append(byAction[i.Action], i.Name)
	}
	b.WriteString("  images:\n")
	for _, act := range []string{ops.ImageBuild, ops.ImagePull, ops.ImageUpToDate, ops.ImageKeep} {
		if names := byAction[act]; len(names) > 0 {
			fmt.Fprintf(&b, "    %-15s %s\n", act+":", strings.Join(names, ", "))
		}
	}
	fmt.Fprintf(&b, "  migrations:  %s", p.Migrations.Status)
	if p.Migrations.Detail != "" {
		fmt.Fprintf(&b, " (%s)", p.Migrations.Detail)
	}
	if p.Migrations.StartedDB {
		b.WriteString("; started the database to check")
	}
	b.WriteString("\n")
	if p.Token.Renew {
		b.WriteString("  token:       renewed\n")
	} else {
		fmt.Fprintf(&b, "  token:       valid until %s\n", p.Token.Expiry.UTC().Format("2006-01-02"))
	}
	if len(p.Restarts) == 0 {
		b.WriteString("  restarts:    none\n")
	} else {
		b.WriteString("  restarts:\n")
		for _, rs := range p.Restarts {
			fmt.Fprintf(&b, "    %-15s %s: %s\n", rs.Service, rs.Action, strings.Join(rs.Reasons, "; "))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func describeCommit(ref, commit, source string) string {
	switch {
	case source != "":
		return shortSHA(commit) + " from " + source
	case commit == "":
		return "(none)"
	case ref == "" || strings.HasPrefix(commit, ref):
		return shortSHA(commit)
	}
	return ref + " (" + shortSHA(commit) + ")"
}
