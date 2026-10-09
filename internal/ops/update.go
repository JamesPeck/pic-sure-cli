package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// Step IDs update adds before up's steps.
const (
	// UpdateConfigStepID backs up and migrates pic-sure.yaml (§9.3 step 3).
	UpdateConfigStepID = "config"
	// UpdateResolveStepID records the release and component commits the
	// plan resolved in state.json.
	UpdateResolveStepID = "resolve"
)

// Migration statuses in an UpdatePlan.
const (
	MigrationsStatusPending  = "pending"
	MigrationsStatusUpToDate = "up_to_date"
	MigrationsStatusUnknown  = "unknown"
)

// Image actions an UpdatePlan lists besides the BuiltImage ones.
const (
	ImageBuild = "build"
	ImagePull  = "pull"
	ImageKeep  = "keep" // --no-build: the stack keeps the image it runs
)

// How a planned restart happens.
const (
	RestartRecreate = "recreate" // compose up recreates it: its definition changes
	RestartRestart  = "restart"  // restarted in place to re-read files or data
)

// migrationsCheck is MigrationsUpToDate, for the plan and the migrate
// step; tests replace it.
var migrationsCheck = MigrationsUpToDate

// UpdateStepIDs are the IDs of UpdateSteps for a stack with config cfg, in
// order, so update can check --skip-step before it takes the lock.
func UpdateStepIDs(cfg *stack.Config) []string {
	return append([]string{UpdateConfigStepID, UpdateResolveStepID, GenomicLeftoversStepID}, planStepIDs(cfg, true)...)
}

// UpdateOptions configure PlanUpdate and UpdateSteps.
type UpdateOptions struct {
	ConvergeOptions
	// Release is the release-control commit to move to, fetched and
	// gated, and Components the commits its components resolve to
	// (release.ResolveComponents). Both are ignored with NoBuild.
	Release    stack.Release
	Components map[string]stack.Component
	// Migrations are the config migrations this pic-sure runs.
	Migrations stack.Registry
	// NoBuild keeps the components and images the stack runs: nothing is
	// built or pulled, and the release isn't moved.
	NoBuild bool
	// StartDB lets the plan start the stack's database to compare the
	// Flyway histories with the migration files.
	StartDB bool
}

// UpdatePlan is §9.3 step 2: what update would change, worked out with
// nothing changed in the stack. It is `update --dry-run --json`'s data,
// and update's report.
type UpdatePlan struct {
	Stack      string            `json:"stack"`
	DryRun     bool              `json:"dry_run"`
	Config     ConfigPlan        `json:"config"`
	Release    ReleaseChange     `json:"release"`
	Components []ComponentChange `json:"components"`
	Images     []ImageChange     `json:"images"`
	Migrations MigrationsPlan    `json:"migrations"`
	Token      TokenPlan         `json:"token"`
	Restarts   []RestartPlan     `json:"restarts"`
	target     *stack.State      // state.json as the resolve step records it
	cfg        *stack.Config     // the config once migrated
	pending    []stack.Migration // the config migrations to run
	trees      map[string]bool   // the bind-mounted trees whose commit changes
	services   map[string]*RestartPlan
}

// ConfigPlan is the pending pic-sure.yaml migrations, From → To.
type ConfigPlan struct {
	From       int      `json:"from"`
	To         int      `json:"to"`
	Migrations []string `json:"migrations"`
}

// ReleaseChange is the release-control commit, current → target.
type ReleaseChange struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// ComponentChange is one component's commit, current → target. Source is
// the local checkout of a component built from one (§7.3), whose commit is
// the checkout's.
type ComponentChange struct {
	Name       string `json:"name"`
	FromRef    string `json:"from_ref,omitempty"`
	FromCommit string `json:"from_commit"`
	ToRef      string `json:"to_ref,omitempty"`
	ToCommit   string `json:"to_commit"`
	Source     string `json:"source,omitempty"`
	Changed    bool   `json:"changed"`
}

// ImageChange is one image's tag, current → target, and what the image
// step will do: build, pull, up_to_date, or keep (--no-build).
type ImageChange struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	From      string `json:"from"`
	To        string `json:"to"`
	Action    string `json:"action"`
}

// MigrationsPlan says whether the Flyway migrations have work to do.
// StartedDB says the plan started the stack's database to find out.
type MigrationsPlan struct {
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	StartedDB bool   `json:"started_db"`
	// dbStopped says the status is unknown only because a database isn't
	// running.
	dbStopped bool
}

// TokenPlan is the introspection token's expiry and whether the seed step
// renews it (it is valid for less than TokenRenewBefore).
type TokenPlan struct {
	Expiry time.Time `json:"expiry,omitzero"`
	Renew  bool      `json:"renew"`
}

// RestartPlan is a running service update recreates or restarts, and why.
type RestartPlan struct {
	Service string   `json:"service"`
	Action  string   `json:"action"`
	Reasons []string `json:"reasons"`
}

// reasonIfMigrationsRun is the reason for a restart that happens only if
// migrations whose status is unknown turn out to be pending.
const reasonIfMigrationsRun = "if the migrations run: they may change what it caches"

// Changes reports whether the plan changes anything at all. Migrations
// whose status is unknown only because a database is stopped (the plan
// didn't start it) don't count, nor do the restarts they would bring: the
// migrate step checks once the database is up.
func (p *UpdatePlan) Changes() bool {
	restarts := slices.ContainsFunc(p.Restarts, func(r RestartPlan) bool {
		return slices.ContainsFunc(r.Reasons, func(s string) bool { return s != reasonIfMigrationsRun })
	})
	if len(p.Config.Migrations) > 0 || p.Release.From != p.Release.To || restarts ||
		p.Migrations.Status != MigrationsStatusUpToDate && !p.Migrations.dbStopped || p.Token.Renew {
		return true
	}
	return slices.ContainsFunc(p.Components, func(c ComponentChange) bool { return c.Changed }) ||
		slices.ContainsFunc(p.Images, func(i ImageChange) bool { return i.Action == ImageBuild || i.Action == ImagePull })
}

// PlanUpdate works out the plan for moving the stack to opts.Release: the
// config migrations, the component commits, the images, the Flyway
// migrations, the token and the services to restart. It writes nothing to
// the stack. With StartDB it may start the stack's
// database, and says so. state is state.json as it is; doc is pic-sure.yaml
// as read, and cfg the config migrated in memory.
func PlanUpdate(ctx context.Context, d *Deps, st *stack.Stack, doc *stack.ConfigDoc, cfg *stack.Config, sec *stack.Secrets, state *stack.State, opts UpdateOptions) (*UpdatePlan, error) {
	p := &UpdatePlan{Stack: cfg.Name, cfg: cfg, services: map[string]*RestartPlan{}, trees: map[string]bool{}}
	var err error
	if p.Config.From, err = doc.Schema(); err != nil {
		return nil, err
	}
	if p.pending, err = opts.Migrations.Plan(doc); err != nil {
		return nil, err
	}
	p.Config.To = opts.Migrations.Target
	p.Config.Migrations = []string{}
	for _, m := range p.pending {
		p.Config.Migrations = append(p.Config.Migrations, fmt.Sprintf("%d→%d: %s", m.From, m.From+1, m.Summary))
	}

	if p.target, err = cloneState(state); err != nil {
		return nil, err
	}
	p.planCommits(st.Dir, cfg, state, opts)
	if err := p.planImages(ctx, d, st, cfg, state, opts); err != nil {
		return nil, err
	}
	if err := p.planMigrations(ctx, d, cfg, sec, opts); err != nil {
		return nil, err
	}
	p.Token.Expiry = sec.IntrospectionTokenExpiry
	p.Token.Renew = string(sec.IntrospectionToken) == "" || !d.Clock.Now().Add(TokenRenewBefore).Before(sec.IntrospectionTokenExpiry)
	if err := p.planRestarts(ctx, d, st, cfg, state, opts); err != nil {
		return nil, err
	}
	return p, nil
}

// planCommits records the target release and component commits in
// p.target, and lists the changes.
func (p *UpdatePlan) planCommits(stackDir string, cfg *stack.Config, state *stack.State, opts UpdateOptions) {
	p.Release = ReleaseChange{Repo: state.Release.Repo, Branch: state.Release.Branch, From: state.Release.Commit, To: state.Release.Commit}
	resolved := opts.Components
	if opts.NoBuild {
		resolved = nil
	} else {
		p.Release = ReleaseChange{Repo: opts.Release.Repo, Branch: opts.Release.Branch, From: state.Release.Commit, To: opts.Release.Commit}
		p.target.Release = opts.Release
	}
	if p.target.Components == nil {
		p.target.Components = map[string]stack.Component{}
	}
	p.Components = []ComponentChange{}
	for _, comp := range catalog.Components() {
		from := state.Components[comp.Name]
		to, ok := resolved[comp.Name]
		if src := componentSource(cfg, comp.Name); src != "" || !ok {
			// A local checkout's commit is read when its images are planned.
			to = from
		}
		p.target.Components[comp.Name] = to
		c := ComponentChange{Name: comp.Name, FromRef: from.Ref, FromCommit: from.Commit, ToRef: to.Ref, ToCommit: to.Commit,
			Source: componentSource(cfg, comp.Name)}
		c.Changed = from.Commit != to.Commit || from.Ref != to.Ref
		if comp.Name == catalog.PicSure || comp.Name == catalog.Migrations {
			// Render mounts a local source in place, whatever --no-build
			// says, and otherwise the cache tree of the commit.
			src := c.Source
			if src != "" && !filepath.IsAbs(src) {
				src = filepath.Join(stackDir, src)
			}
			if src != from.Source || src == "" && from.Commit != to.Commit {
				p.trees[comp.Name] = true
			}
		}
		p.Components = append(p.Components, c)
	}
}

// planImages lists every image's current and target tag and what the
// image step will do with it. It records the target tags in p.target.
func (p *UpdatePlan) planImages(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts UpdateOptions) error {
	p.Images = []ImageChange{}
	if opts.NoBuild {
		for _, comp := range catalog.Components() {
			for _, img := range catalog.ImagesBuiltFrom(comp.Name) {
				tag := state.Images[img.Name]
				p.Images = append(p.Images, ImageChange{Name: img.Name, Component: comp.Name, From: tag, To: tag, Action: ImageKeep})
			}
		}
		return nil
	}
	parts, err := planImages(ctx, d, st, cfg, p.target, ImagesOptions{Cache: opts.Cache})
	if err != nil {
		return err
	}
	for _, part := range parts {
		// A local checkout's commit is only known now.
		if part.comp.Source != "" {
			p.target.Components[part.component] = part.comp
			for i := range p.Components {
				if c := &p.Components[i]; c.Name == part.component {
					c.ToCommit, c.ToRef = part.comp.Commit, ""
					c.Changed = c.FromCommit != c.ToCommit || c.FromRef != ""
				}
			}
		}
		action := ImageBuild
		switch {
		case part.pull:
			// Update pulls every time: a ref such as a branch can move.
			action = ImagePull
		default:
			upToDate, err := part.upToDate(ctx, d, opts.Cache, cfg, p.target)
			if err != nil {
				return err
			}
			if upToDate {
				action = ImageUpToDate
			}
		}
		part.record(p.target, cfg, part.tag)
		for _, img := range part.images {
			p.Images = append(p.Images, ImageChange{Name: img.Name, Component: part.component,
				From: state.Images[img.Name], To: part.tag, Action: action})
		}
	}
	return nil
}

// planMigrations compares the Flyway histories with the migration files
// the stack mounts. When the render will mount them from somewhere else
// (another commit's tree, or a local source set or unset), the current
// mounts say nothing about them, so the migrate step's Check decides
// after the render.
func (p *UpdatePlan) planMigrations(ctx context.Context, d *Deps, cfg *stack.Config, sec *stack.Secrets, opts UpdateOptions) error {
	if len(p.trees) > 0 {
		p.Migrations = MigrationsPlan{Status: MigrationsStatusUnknown,
			Detail: "the migration files come from a new " + strings.Join(slices.Sorted(maps.Keys(p.trees)), " and ") +
				" tree; the migrate step compares them with the Flyway histories after the render"}
		return nil
	}
	if err := ensureCompose(d, opts.ConvergeOptions); err != nil {
		return err
	}
	healthy := func(service string) (bool, error) {
		svc, err := composeService(ctx, d, service)
		return err == nil && svc != nil && svc.State == "running" && svc.Health == "healthy", err
	}
	// The comparison needs both databases; without dictionary-db, starting
	// picsure-db would tell nothing.
	if ok, err := healthy(dictionaryDB); err != nil || !ok {
		p.Migrations.Status, p.Migrations.Detail = MigrationsStatusUnknown, dictionaryDB+" isn't running; the migrate step checks once it is"
		p.Migrations.dbStopped = err == nil
		return err
	}
	if cfg.DB.Mode != stack.DBRemote {
		ok, err := healthy(picsureDB)
		if err != nil {
			return err
		}
		if !ok {
			if !opts.StartDB {
				p.Migrations = MigrationsPlan{Status: MigrationsStatusUnknown, Detail: picsureDB + " isn't running", dbStopped: true}
				return nil
			}
			if err := steps.Run(ctx, d.Sink, DBSteps(d, cfg, sec, DBOptions{}), steps.Options{}); err != nil {
				return fmt.Errorf("starting %s to compare the migrations: %w", picsureDB, err)
			}
			p.Migrations.StartedDB = true
		}
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	ok, err := migrationsCheck(cctx, d, cfg, sec)
	switch {
	case err != nil:
		p.Migrations.Status, p.Migrations.Detail = MigrationsStatusUnknown, err.Error()
	case ok:
		p.Migrations.Status = MigrationsStatusUpToDate
	default:
		p.Migrations.Status = MigrationsStatusPending
	}
	return nil
}

// planRestarts lists the running services update recreates or restarts:
// those whose compose definition the new render changes, the readers of
// rendered files it changes, httpd and psama when their TLS or truststore
// volume is refilled, the services that cache what migrations and seeding
// write, the ones that read the introspection token when it is renewed,
// and the restarts an earlier run left pending.
func (p *UpdatePlan) planRestarts(ctx context.Context, d *Deps, st *stack.Stack, cfg *stack.Config, state *stack.State, opts UpdateOptions) error {
	p.Restarts = []RestartPlan{}
	if err := ensureCompose(d, opts.ConvergeOptions); err != nil {
		return err
	}
	running, err := d.Compose.Ps(ctx)
	if err != nil {
		return err
	}
	isRunning := map[string]bool{}
	for _, c := range running {
		if c.State == "running" {
			isRunning[c.Service] = true
		}
	}
	add := func(svc, action, reason string) {
		if !isRunning[svc] {
			return
		}
		r := p.services[svc]
		if r == nil {
			r = &RestartPlan{Service: svc, Action: action}
			p.services[svc] = r
		}
		if action == RestartRecreate {
			r.Action = RestartRecreate
		}
		if !slices.Contains(r.Reasons, reason) {
			r.Reasons = append(r.Reasons, reason)
		}
	}

	files, err := renderStack(ctx, d, st, cfg, p.target, opts.Cache)
	if err != nil {
		return err
	}
	var newCompose []byte
	for _, f := range files {
		if f.Path == render.ComposeFile {
			newCompose = f.Data
		}
	}
	recreated, err := recreatedServices(ctx, d, newCompose, running, opts.Cache)
	if err != nil {
		return err
	}
	for _, svc := range recreated {
		add(svc, RestartRecreate, "its compose config changes")
	}
	for _, img := range p.Images {
		reason := map[string]string{ImageBuild: "its image is rebuilt", ImagePull: "if the pull brings a new image"}[img.Action]
		if reason == "" {
			continue
		}
		for _, svc := range catalog.ServicesUsing(img.Name) {
			add(svc, RestartRecreate, reason)
		}
	}
	tokenReaders, err := tokenReaders(newCompose)
	if err != nil {
		return err
	}
	if p.Token.Renew {
		for _, svc := range tokenReaders {
			add(svc, RestartRecreate, "the introspection token is renewed")
		}
		add(psama, RestartRestart, "the introspection token is renewed")
	}

	changedFiles, err := changedRenderFiles(st, files)
	if err != nil {
		return err
	}
	if len(changedFiles) > 0 {
		r := &upRestarts{d: d, st: st, cfg: cfg, opts: opts.ConvergeOptions}
		readers, err := r.readers(ctx, changedFiles)
		if err != nil {
			readers = StartServices(cfg)
		}
		for _, svc := range readers {
			add(svc, RestartRestart, "a file it mounts changes")
		}
	}

	if ok, err := TLSStep(d, st, cfg).Check(ctx); err != nil || !ok {
		add(httpd, RestartRestart, "its TLS certificate is reinstalled")
	}
	if ok, err := StackTruststoreStep(d, st, cfg, p.target).Check(ctx); err != nil || !ok {
		add(psama, RestartRestart, "its truststore is rebuilt")
	}
	if p.Migrations.Status != MigrationsStatusUpToDate {
		reason := "the migrations change what it caches"
		if p.Migrations.Status == MigrationsStatusUnknown {
			reason = reasonIfMigrationsRun
		}
		for _, s := range catalog.Services() {
			if s.RestartAfterMigrate {
				add(s.Name, RestartRestart, reason)
			}
		}
	}
	for _, svc := range state.PendingRestarts {
		add(svc, RestartRestart, "an earlier run left it to restart")
	}
	for _, svc := range slices.Sorted(maps.Keys(p.services)) {
		p.Restarts = append(p.Restarts, *p.services[svc])
	}
	return nil
}

// recreatedServices returns the running services `compose up` would
// recreate at the new render: those whose config hash, with the stack's
// env, differs from the one compose labelled the container with. That
// covers a changed definition, image tag or env value, and a render an
// earlier run wrote but never started. The new compose.yaml goes to a
// temporary file in the cache, so the stack is untouched.
func recreatedServices(ctx context.Context, d *Deps, compose []byte, running []docker.ComposeService, c *cache.Cache) ([]string, error) {
	cur, ok := d.Compose.(*docker.Compose)
	if !ok {
		return nil, fmt.Errorf("planning the restarts needs the compose adapter, not %T", d.Compose)
	}
	dir, err := c.TempDir("update-plan-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, compose, 0o600); err != nil {
		return nil, err
	}
	hashes, err := cur.ConfigHashes(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("hashing the new compose config: %w", err)
	}
	var out []string
	for _, svc := range running {
		if h, ok := hashes[svc.Service]; ok && svc.State == "running" && h != svc.Label(docker.ConfigHashLabel) {
			out = append(out, svc.Service)
		}
	}
	return out, nil
}

// tokenReaders returns the services of a compose.yaml whose definition
// reads the introspection token, which compose passes in the env.
func tokenReaders(compose []byte) ([]string, error) {
	var doc struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		return nil, err
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(doc.Services)) {
		if mentions(doc.Services[name], "${PICSURE_INTROSPECTION_TOKEN}") {
			out = append(out, name)
		}
	}
	return out, nil
}

// mentions reports whether s occurs in a string anywhere in v, a decoded
// YAML value.
func mentions(v any, s string) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, s)
	case map[string]any:
		for _, e := range v {
			if mentions(e, s) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if mentions(e, s) {
				return true
			}
		}
	}
	return false
}

// changedRenderFiles returns the absolute paths under render/files that
// rendering files would change, add or remove.
func changedRenderFiles(st *stack.Stack, files []render.File) ([]string, error) {
	before, err := readTree(st.Path(render.FilesDir))
	if err != nil {
		return nil, err
	}
	after := map[string][]byte{}
	for _, f := range files {
		if strings.HasPrefix(f.Path, render.FilesDir+"/") {
			after[st.Path(f.Path)] = f.Data
		}
	}
	var out []string
	for path, data := range after {
		if old, ok := before[path]; !ok || !bytes.Equal(old, data) {
			out = append(out, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out, nil
}

// cloneState deep-copies a state through its JSON form.
func cloneState(s *stack.State) (*stack.State, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	out := &stack.State{}
	return out, json.Unmarshal(data, out)
}

// UpdateSteps are §9.3 steps 3 to 7 for the plan: back up and migrate
// pic-sure.yaml, record the release and component commits the plan
// resolved, then up's steps with the image step refreshing pulled images.
// cfg is the config migrated in memory, as the plan used it. With NoBuild
// the caller skips the image step.
func UpdateSteps(d *Deps, st *stack.Stack, plan *UpdatePlan, sec *stack.Secrets, state *stack.State, opts UpdateOptions) []steps.Step {
	cfg := plan.cfg
	config := steps.Step{
		ID:    UpdateConfigStepID,
		Title: "Back up and migrate pic-sure.yaml",
		Check: func(context.Context) (bool, error) {
			doc, err := st.ReadConfigDoc()
			if err != nil {
				return false, err
			}
			pending, err := opts.Migrations.Plan(doc)
			return len(pending) == 0, err
		},
		Apply: func(_ context.Context, sink events.Sink) error {
			applied, err := opts.Migrations.Apply(st, d.Clock.Now())
			if err != nil {
				return err
			}
			if applied.BackupDir != "" {
				sink.Emit(events.Progress{ID: UpdateConfigStepID, Text: fmt.Sprintf("migrated %s to schema %d; the old files are in %s",
					stack.ConfigFile, opts.Migrations.Target, applied.BackupDir)})
			}
			return nil
		},
	}
	resolve := steps.Step{
		ID:    UpdateResolveStepID,
		Title: "Record the release's component commits",
		Check: func(context.Context) (bool, error) {
			return state.Release == plan.target.Release && reflect.DeepEqual(releaseComponents(cfg, state), releaseComponents(cfg, plan.target)), nil
		},
		Apply: func(context.Context, events.Sink) error {
			state.Release = plan.target.Release
			if state.Components == nil {
				state.Components = map[string]stack.Component{}
			}
			maps.Copy(state.Components, releaseComponents(cfg, plan.target))
			return st.SaveState(state)
		},
	}
	images := ImagesStep(d, st, cfg, state, ImagesOptions{Cache: opts.Cache, Refresh: true})
	if cfg.Images.Mode == stack.ImagesPull {
		// A ref such as a branch can move, so pull mode always pulls.
		images.Check = nil
	}
	return append([]steps.Step{config, resolve, genomicLeftoversStep(d, st, cfg)}, planSteps(d, st, cfg, sec, state, opts.ConvergeOptions, images, true)...)
}

// releaseComponents are state's components that aren't built from a local
// source: the ones the release decides.
func releaseComponents(cfg *stack.Config, state *stack.State) map[string]stack.Component {
	out := map[string]stack.Component{}
	for name, c := range state.Components {
		if componentSource(cfg, name) == "" {
			out[name] = c
		}
	}
	return out
}
