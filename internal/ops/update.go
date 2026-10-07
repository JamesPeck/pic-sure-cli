package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
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
	MigrationsStatusUpToDate = "up-to-date"
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

// migrationsCheck is MigrationsUpToDate; tests replace it.
var migrationsCheck = MigrationsUpToDate

// UpdateStepIDs are the IDs of UpdateSteps for a stack with config cfg, in
// order, so update can check --skip-step before it takes the lock.
func UpdateStepIDs(cfg *stack.Config) []string {
	return append([]string{UpdateConfigStepID, UpdateResolveStepID}, UpStepIDs(cfg)...)
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
// step will do: build, pull, up-to-date, or keep (--no-build).
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

// Changes reports whether the plan changes anything at all.
func (p *UpdatePlan) Changes() bool {
	if len(p.Config.Migrations) > 0 || p.Release.From != p.Release.To || len(p.Restarts) > 0 ||
		p.Migrations.Status != MigrationsStatusUpToDate || p.Token.Renew {
		return true
	}
	return slices.ContainsFunc(p.Components, func(c ComponentChange) bool { return c.Changed }) ||
		slices.ContainsFunc(p.Images, func(i ImageChange) bool { return i.Action == ImageBuild || i.Action == ImagePull })
}

// PlanUpdate works out the plan for moving the stack to opts.Release: the
// config migrations, the component commits, the
// images, the Flyway migrations, the token and the services to restart. It
// writes nothing to the stack. With StartDB it may start the stack's
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
	p.planCommits(cfg, state, opts)
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
func (p *UpdatePlan) planCommits(cfg *stack.Config, state *stack.State, opts UpdateOptions) {
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
		if c.Changed && (comp.Name == catalog.PicSure || comp.Name == catalog.Migrations) {
			p.trees[comp.Name] = true
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
		upToDate, err := part.upToDate(ctx, d, opts.Cache, cfg, p.target)
		if err != nil {
			return err
		}
		action := ImageBuild
		switch {
		case upToDate:
			action = ImageUpToDate
		case part.pull:
			action = ImagePull
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
// the stack mounts. When the trees holding them move to another commit,
// the new files aren't in the cache yet, so the migrate step's Check
// decides after the image step.
func (p *UpdatePlan) planMigrations(ctx context.Context, d *Deps, cfg *stack.Config, sec *stack.Secrets, opts UpdateOptions) error {
	if len(p.trees) > 0 {
		p.Migrations = MigrationsPlan{Status: MigrationsStatusUnknown,
			Detail: "the migration files move to " + strings.Join(slices.Sorted(maps.Keys(p.trees)), " and ") +
				"'s new commit; the migrate step compares them with the Flyway histories once the image step has fetched it"}
		return nil
	}
	if err := ensureCompose(d, opts.ConvergeOptions); err != nil {
		return err
	}
	if cfg.DB.Mode != stack.DBRemote {
		svc, err := composeService(ctx, d, picsureDB)
		if err != nil {
			return err
		}
		if svc == nil || svc.State != "running" || svc.Health != "healthy" {
			if !opts.StartDB {
				p.Migrations = MigrationsPlan{Status: MigrationsStatusUnknown, Detail: picsureDB + " isn't running"}
				return nil
			}
			if err := steps.Run(ctx, d.Sink, DBSteps(d, cfg, sec, DBOptions{}), steps.Options{}); err != nil {
				return fmt.Errorf("starting %s to compare the migrations: %w", picsureDB, err)
			}
			p.Migrations.StartedDB = true
		}
	}
	dict, err := composeService(ctx, d, dictionaryDB)
	if err != nil {
		return err
	}
	if dict == nil || dict.State != "running" || dict.Health != "healthy" {
		p.Migrations.Status, p.Migrations.Detail = MigrationsStatusUnknown, dictionaryDB+" isn't running; the migrate step checks once it is"
		return nil
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

	files, err := renderStack(st, cfg, p.target, opts.Cache)
	if err != nil {
		return err
	}
	var newCompose []byte
	for _, f := range files {
		if f.Path == render.ComposeFile {
			newCompose = f.Data
		}
	}
	oldCompose, err := os.ReadFile(st.Path(render.ComposeFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	changed, tokenReaders, err := composeChanges(oldCompose, newCompose)
	if err != nil {
		return err
	}
	for _, svc := range changed {
		add(svc, RestartRecreate, "its compose definition changes")
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
			reason = "if the migrations run: they may change what it caches"
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

// composeChanges compares two renders of compose.yaml: the services the
// new one adds or defines differently, and the services whose definition
// reads the introspection token.
func composeChanges(oldData, newData []byte) (changed, tokenReaders []string, err error) {
	var oldDoc, newDoc struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(oldData, &oldDoc); err != nil {
		return nil, nil, fmt.Errorf("reading the current %s: %w", render.ComposeFile, err)
	}
	if err := yaml.Unmarshal(newData, &newDoc); err != nil {
		return nil, nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(newDoc.Services)) {
		def := newDoc.Services[name]
		if old, ok := oldDoc.Services[name]; !ok || !reflect.DeepEqual(old, def) {
			changed = append(changed, name)
		}
		if mentions(def, "${PICSURE_INTROSPECTION_TOKEN}") {
			tokenReaders = append(tokenReaders, name)
		}
	}
	return changed, tokenReaders, nil
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
	return append([]steps.Step{config, resolve}, upSteps(d, st, cfg, sec, state, opts.ConvergeOptions, images)...)
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
