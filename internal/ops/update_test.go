package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const (
	relSHA   = "5555555555555555555555555555555555555555"
	relSHA2  = "6666666666666666666666666666666666666666"
	feSHA2   = "7777777777777777777777777777777777777777"
	migSHA2  = "8888888888888888888888888888888888888888"
	tokenVar = "tok-synthetic"
)

// updateFixture is a build fixture whose stack is rendered and running at
// the recorded release, with every image present and the migrations up to
// date unless a test says otherwise.
type updateFixture struct {
	*buildFixture
	sec        *stack.Secrets
	running    []string
	migrations bool
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	x := &updateFixture{buildFixture: newBuildFixture(t), migrations: true}
	x.d.Clock = ops.FixedClock(t0)
	x.sec = &stack.Secrets{IntrospectionToken: tokenVar, IntrospectionTokenExpiry: t0.Add(300 * 24 * time.Hour)}
	x.state.Release = stack.Release{Repo: "https://example.com/rc.git", Branch: "main", Commit: relSHA}
	x.state.Images = map[string]string{"pic-sure-httpd": x.feTag(), "dictionary-etl": etlSHA[:12]}
	for _, img := range catalog.ImagesBuiltFrom(catalog.PicSure) {
		x.state.Images[img.Name] = psSHA[:12]
	}
	if err := x.st.SaveState(x.state); err != nil {
		t.Fatal(err)
	}
	doc, err := stack.NewConfigDoc(x.cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := x.st.WriteConfig(data); err != nil {
		t.Fatal(err)
	}
	x.installTLS()
	opts := ops.ConvergeOptions{Cache: x.cache, Compose: x.compose}
	if err := ops.RenderStep(x.d, x.st, x.cfg, x.state, opts).Apply(context.Background(), x.rec); err != nil {
		t.Fatal(err)
	}
	x.running = ops.StartServices(x.cfg)
	x.f.On(fakerunner.Glob("docker compose * ps *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		var b strings.Builder
		only := c.Argv[len(c.Argv)-1]
		for _, s := range x.running {
			if strings.HasPrefix(only, "-") || only == "json" || only == s {
				b.WriteString(psLine(s, "running", "healthy"))
			}
		}
		return docker.Result{Stdout: []byte(b.String())}, nil
	})
	x.reactorFresh(psSHA[:12], psSHA)
	x.frontendFresh()
	x.etlFresh(etlSHA[:12], etlSHA)
	check := *ops.MigrationsCheck
	*ops.MigrationsCheck = func(context.Context, *ops.Deps, *stack.Config, *stack.Secrets) (bool, error) {
		return x.migrations, nil
	}
	t.Cleanup(func() { *ops.MigrationsCheck = check })
	return x
}

// installTLS runs the TLS step against a fake certs volume, so its Check
// is done.
func (x *updateFixture) installTLS() {
	x.t.Helper()
	var labels map[string]string
	x.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_certs")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if labels == nil {
			return docker.Result{Stderr: []byte("Error response from daemon: get demo_certs: no such volume\n"), ExitCode: 1}, nil
		}
		out, err := json.Marshal([]map[string]any{{"Name": "demo_certs", "Driver": "local", "CreatedAt": "2026-10-06T12:00:00Z", "Labels": labels}})
		return docker.Result{Stdout: out}, err
	})
	x.f.On(fakerunner.Glob("docker volume create * demo_certs")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		labels = map[string]string{}
		for i, a := range c.Argv {
			if a == "--label" {
				k, v, _ := strings.Cut(c.Argv[i+1], "=")
				labels[k] = v
			}
		}
		return docker.Result{}, nil
	})
	x.f.On(fakerunner.Glob("docker run -i --rm --name demo-tls-* alpine:* sh -c *"))
	rnd := x.d.Rand
	x.d.Rand = rand.Reader
	defer func() { x.d.Rand = rnd }()
	if err := ops.TLSStep(x.d, x.st, x.cfg).Apply(context.Background(), x.rec); err != nil {
		x.t.Fatal(err)
	}
	state, err := x.st.LoadState()
	if err != nil {
		x.t.Fatal(err)
	}
	x.state.TLS = state.TLS
}

func (x *updateFixture) compose() (docker.Composer, error) {
	return docker.NewCompose(x.f, x.st.Dir, nil)
}

// target is the recorded release and commits, with changes applied.
func (x *updateFixture) target(changes map[string]stack.Component) (stack.Release, map[string]stack.Component) {
	comps := map[string]stack.Component{}
	for k, v := range x.state.Components {
		comps[k] = v
	}
	for k, v := range changes {
		comps[k] = v
	}
	rel := x.state.Release
	if len(changes) > 0 {
		rel.Commit = relSHA2
	}
	return rel, comps
}

func (x *updateFixture) plan(opts ops.UpdateOptions) *ops.UpdatePlan {
	x.t.Helper()
	doc, err := stack.NewConfigDoc(x.cfg)
	if err != nil {
		x.t.Fatal(err)
	}
	if opts.Migrations.Target == 0 {
		opts.Migrations = stack.ConfigMigrations()
	}
	opts.Cache, opts.Compose = x.cache, x.compose
	before, err := os.ReadFile(x.st.Path(stack.StateFile))
	if err != nil {
		x.t.Fatal(err)
	}
	p, err := ops.PlanUpdate(context.Background(), x.d, x.st, doc, x.cfg, x.sec, x.state, opts)
	if err != nil {
		x.t.Fatal(err)
	}
	after, err := os.ReadFile(x.st.Path(stack.StateFile))
	if err != nil {
		x.t.Fatal(err)
	}
	if string(before) != string(after) {
		x.t.Error("the plan changed state.json")
	}
	return p
}

func restarts(p *ops.UpdatePlan) map[string]string {
	out := map[string]string{}
	for _, r := range p.Restarts {
		out[r.Service] = r.Action
	}
	return out
}

func imageActions(p *ops.UpdatePlan) map[string]string {
	out := map[string]string{}
	for _, i := range p.Images {
		out[i.Name] = i.Action
	}
	return out
}

func TestPlanUpdateOfACurrentStackChangesNothing(t *testing.T) {
	x := newUpdateFixture(t)
	rel, comps := x.target(nil)
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps})
	if p.Changes() {
		t.Errorf("the plan has changes: %+v", p)
	}
	if len(p.Restarts) != 0 {
		t.Errorf("restarts %+v, want none", p.Restarts)
	}
	if p.Release.From != relSHA || p.Release.To != relSHA {
		t.Errorf("release %+v, want %s unchanged", p.Release, relSHA)
	}
	for name, a := range imageActions(p) {
		if a != ops.ImageUpToDate {
			t.Errorf("%s: %s, want up-to-date", name, a)
		}
	}
	if p.Migrations.Status != ops.MigrationsStatusUpToDate || p.Migrations.StartedDB {
		t.Errorf("migrations %+v, want up to date without starting the database", p.Migrations)
	}
	if len(p.Config.Migrations) != 0 || p.Config.From != stack.ConfigSchema || p.Config.To != stack.ConfigSchema {
		t.Errorf("config %+v, want nothing to migrate", p.Config)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
}

func TestPlanUpdateRebuildsAndRecreatesOnlyWhatAComponentChangeAffects(t *testing.T) {
	x := newUpdateFixture(t)
	x.missing()
	rel, comps := x.target(map[string]stack.Component{catalog.Frontend: {Ref: "v2.1", Commit: feSHA2}})
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps})

	if p.Release.From != relSHA || p.Release.To != relSHA2 {
		t.Errorf("release %+v, want %s → %s", p.Release, relSHA, relSHA2)
	}
	for _, c := range p.Components {
		if c.Changed != (c.Name == catalog.Frontend) {
			t.Errorf("%s changed = %v", c.Name, c.Changed)
		}
		if c.Name == catalog.Frontend && (c.FromCommit != feSHA || c.ToCommit != feSHA2 || c.ToRef != "v2.1") {
			t.Errorf("frontend %+v, want %s → v2.1 %s", c, feSHA, feSHA2)
		}
	}
	for name, a := range imageActions(p) {
		if want := map[bool]string{true: ops.ImageBuild, false: ops.ImageUpToDate}[name == "pic-sure-httpd"]; a != want {
			t.Errorf("%s: %s, want %s", name, a, want)
		}
	}
	if got := restarts(p); len(got) != 1 || got["httpd"] != ops.RestartRecreate {
		t.Errorf("restarts %v, want httpd recreated only", p.Restarts)
	}
	if p.Migrations.Status != ops.MigrationsStatusUpToDate {
		t.Errorf("migrations %+v, want up to date", p.Migrations)
	}
}

func TestPlanUpdateLeavesMovedMigrationsToTheMigrateStep(t *testing.T) {
	x := newUpdateFixture(t)
	x.missing()
	rel, comps := x.target(map[string]stack.Component{catalog.Migrations: {Ref: "v3.1", Commit: migSHA2}})
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps})
	if p.Migrations.Status != ops.MigrationsStatusUnknown || !strings.Contains(p.Migrations.Detail, "migrations's new commit") {
		t.Errorf("migrations %+v, want unknown until the new tree is fetched", p.Migrations)
	}
	// flyway-init's mounts move, but it is a one-shot, so only the services
	// that cache what migrations write may restart.
	want := map[string]string{"psama": ops.RestartRestart, "dictionary-api": ops.RestartRestart}
	if got := restarts(p); len(got) != len(want) || got["psama"] != want["psama"] || got["dictionary-api"] != want["dictionary-api"] {
		t.Errorf("restarts %+v, want %v", p.Restarts, want)
	}
}

func TestPlanUpdateListsPendingMigrationsAndTheirRestarts(t *testing.T) {
	x := newUpdateFixture(t)
	x.migrations = false
	rel, comps := x.target(nil)
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps})
	if p.Migrations.Status != ops.MigrationsStatusPending {
		t.Errorf("migrations %+v, want pending", p.Migrations)
	}
	if got := restarts(p); got["psama"] != ops.RestartRestart || got["dictionary-api"] != ops.RestartRestart {
		t.Errorf("restarts %+v, want psama and dictionary-api", p.Restarts)
	}
	if !p.Changes() {
		t.Error("pending migrations are no change")
	}
}

func TestPlanUpdateDoesntStartTheDatabaseUnlessAllowed(t *testing.T) {
	x := newUpdateFixture(t)
	x.running = slices.DeleteFunc(x.running, func(s string) bool { return s == "picsure-db" })
	rel, comps := x.target(nil)
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps})
	if p.Migrations.Status != ops.MigrationsStatusUnknown || p.Migrations.StartedDB {
		t.Errorf("migrations %+v, want unknown with the database left stopped", p.Migrations)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker compose * up *"))
}

func TestPlanUpdateRenewsATokenNearItsExpiry(t *testing.T) {
	x := newUpdateFixture(t)
	x.sec.IntrospectionTokenExpiry = t0.Add(10 * 24 * time.Hour)
	rel, comps := x.target(nil)
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps})
	if !p.Token.Renew {
		t.Error("a token expiring in 10 days isn't renewed")
	}
	if got := restarts(p); got["gateway"] != ops.RestartRecreate || got["psama"] != ops.RestartRestart {
		t.Errorf("restarts %+v, want gateway recreated for its env and psama restarted", p.Restarts)
	}
}

func TestPlanUpdateRestartsTheReadersOfAChangedFileAndPendingRestarts(t *testing.T) {
	x := newUpdateFixture(t)
	vhosts := x.st.Path(render.FilesDir + "/httpd/httpd-vhosts.conf")
	if err := os.WriteFile(vhosts, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	x.f.On(fakerunner.Glob("docker compose * config --no-interpolate")).Stdout("services:\n  httpd:\n    volumes:\n      - {type: bind, source: " + vhosts + ", target: /conf}\n")
	x.state.PendingRestarts = []string{"dictionary-api"}
	rel, comps := x.target(nil)
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps})
	want := map[string]string{"httpd": ops.RestartRestart, "dictionary-api": ops.RestartRestart}
	if got := restarts(p); len(got) != 2 || got["httpd"] != want["httpd"] || got["dictionary-api"] != want["dictionary-api"] {
		t.Errorf("restarts %+v, want %v", p.Restarts, want)
	}
}

func TestPlanUpdateNoBuildKeepsTheRelease(t *testing.T) {
	x := newUpdateFixture(t)
	rel, comps := x.target(map[string]stack.Component{catalog.Frontend: {Ref: "v2.1", Commit: feSHA2}})
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps, NoBuild: true})
	if p.Release.To != relSHA || slices.ContainsFunc(p.Components, func(c ops.ComponentChange) bool { return c.Changed }) {
		t.Errorf("release %+v, components %+v: --no-build moved them", p.Release, p.Components)
	}
	for name, a := range imageActions(p) {
		if a != ops.ImageKeep {
			t.Errorf("%s: %s, want keep", name, a)
		}
	}
	if len(p.Restarts) != 0 {
		t.Errorf("restarts %+v, want none", p.Restarts)
	}
}

func TestPlanUpdateListsPendingConfigMigrations(t *testing.T) {
	x := newUpdateFixture(t)
	reg := stack.Registry{Target: stack.ConfigSchema + 1, Steps: []stack.Migration{{
		From: stack.ConfigSchema, Summary: "rename a key", Apply: func(*yaml.Node) error { return nil },
	}}}
	rel, comps := x.target(nil)
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps, Migrations: reg})
	if p.Config.From != stack.ConfigSchema || p.Config.To != stack.ConfigSchema+1 ||
		!slices.Equal(p.Config.Migrations, []string{"1→2: rename a key"}) {
		t.Errorf("config %+v", p.Config)
	}
	if !p.Changes() {
		t.Error("a config migration is no change")
	}
}

func TestUpdateStepIDsMatchUpdateSteps(t *testing.T) {
	x := newUpdateFixture(t)
	rel, comps := x.target(nil)
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps})
	var ids []string
	for _, s := range ops.UpdateSteps(x.d, x.st, p, x.sec, x.state, ops.UpdateOptions{}) {
		ids = append(ids, s.ID)
	}
	if want := ops.UpdateStepIDs(x.cfg); !slices.Equal(ids, want) {
		t.Errorf("UpdateSteps %v, UpdateStepIDs %v", ids, want)
	}
	if want := append([]string{ops.UpdateConfigStepID, ops.UpdateResolveStepID}, ops.UpStepIDs(x.cfg)...); !slices.Equal(ids, want) {
		t.Errorf("UpdateSteps %v, want config and resolve before up's", ids)
	}
}

func TestUpdateRecordsTheTargetBeforeTheImageStep(t *testing.T) {
	x := newUpdateFixture(t)
	x.missing()
	reg := stack.Registry{Target: stack.ConfigSchema, Steps: nil}
	rel, comps := x.target(map[string]stack.Component{catalog.Frontend: {Ref: "v2.1", Commit: feSHA2}})
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps, Migrations: reg})
	list := ops.UpdateSteps(x.d, x.st, p, x.sec, x.state, ops.UpdateOptions{Migrations: reg})
	for _, s := range list[:2] {
		if s.ID == ops.UpdateConfigStepID {
			if done, err := s.Check(context.Background()); err != nil || !done {
				t.Errorf("config Check = %v, %v with nothing to migrate", done, err)
			}
			continue
		}
		if done, _ := s.Check(context.Background()); done {
			t.Fatal("resolve Check is done before the commits are recorded")
		}
		if err := s.Apply(context.Background(), x.rec); err != nil {
			t.Fatal(err)
		}
		if done, _ := s.Check(context.Background()); !done {
			t.Error("resolve Check isn't done after Apply")
		}
	}
	saved, err := x.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Release.Commit != relSHA2 || saved.Components[catalog.Frontend].Commit != feSHA2 || saved.Components[catalog.PicSure].Commit != psSHA {
		t.Errorf("saved release %+v, components %+v", saved.Release, saved.Components)
	}
	// The image step builds the new frontend; until then the stack keeps
	// the image it runs.
	if saved.Images["pic-sure-httpd"] != x.feTag() {
		t.Errorf("httpd image %q recorded before it is built", saved.Images["pic-sure-httpd"])
	}
}
