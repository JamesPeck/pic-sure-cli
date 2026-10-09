package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// planFixture is updateFixture's running, current stack with the rest of
// its host faked, so the whole init, up and update plans run: picsure-db
// answers the seed's SQL, the HPDS key volume holds the key, and compose
// up and run change what ps and the migrations check report.
type planFixture struct {
	*updateFixture
	db       *fakeDB
	hpds     *hpdsVolume
	spy      *upEnvSpy
	composes int
}

// upEnvSpy records the env of each `compose up`, of which fakerunner keeps
// only the names.
type upEnvSpy struct {
	docker.Runner
	mu    sync.Mutex
	upEnv [][]string
}

func (s *upEnvSpy) record(c docker.Cmd) {
	if slices.Contains(c.Argv, "compose") && slices.Contains(c.Argv, "up") {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.upEnv = append(s.upEnv, slices.Clone(c.Env))
	}
}

func (s *upEnvSpy) Run(ctx context.Context, c docker.Cmd) (docker.Result, error) {
	s.record(c)
	return s.Runner.Run(ctx, c)
}

func (s *upEnvSpy) Stream(ctx context.Context, c docker.Cmd, stdout, stderr io.Writer) (int, error) {
	s.record(c)
	return s.Runner.Stream(ctx, c, stdout, stderr)
}

func newPlanFixture(t *testing.T) *planFixture {
	t.Helper()
	x := &planFixture{updateFixture: newUpdateFixture(t), db: newFakeDB(t), hpds: &hpdsVolume{}}
	x.spy = &upEnvSpy{Runner: x.f}
	sec, err := x.st.EnsureSecrets(rand.Reader, stack.EnsureOptions{OpenAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	sec.IntrospectionToken, sec.IntrospectionTokenExpiry = x.sec.IntrospectionToken, x.sec.IntrospectionTokenExpiry
	if err := x.st.SaveSecrets(sec); err != nil {
		t.Fatal(err)
	}
	x.sec = sec
	x.db.token = string(sec.IntrospectionToken)

	x.f.On(mysqlExec).Do(func(ctx context.Context, c fakerunner.Call) (docker.Result, error) {
		if strings.HasSuffix(strings.TrimSpace(string(c.Stdin)), "SELECT 1\n;") {
			return docker.Result{Stdout: []byte("1\n")}, nil
		}
		return x.db.do(ctx, c)
	})
	// No genomic store, so nothing an interrupted promote left.
	x.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-genomic")).Exit(1).
		Stderr("Error response from daemon: get demo_hpds-genomic: no such volume\n")
	x.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-data")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if x.hpds.createdAt == "" {
			return docker.Result{Stderr: []byte("Error response from daemon: get demo_hpds-data: no such volume\n"), ExitCode: 1}, nil
		}
		out, err := json.Marshal([]map[string]any{{"Name": "demo_hpds-data", "CreatedAt": x.hpds.createdAt, "Labels": x.st.Labels("demo")}})
		return docker.Result{Stdout: out}, err
	})
	x.f.On(fakerunner.Glob("docker volume create * demo_hpds-data")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		x.hpds.createdAt = "2026-10-07T12:00:00Z"
		return docker.Result{}, nil
	})
	x.f.On(fakerunner.Glob("docker run -i --rm --name demo-hpds-key-* alpine:* sh -c *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		x.hpds.runs++
		x.hpds.key = c.Stdin
		return docker.Result{}, nil
	})
	x.f.On(fakerunner.Glob("docker compose * up *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		if slices.Contains(c.Argv, "--wait") {
			x.running = ops.StartServices(x.cfg)
		} else if slices.Contains(c.Argv, "picsure-db") && !slices.Contains(x.running, "picsure-db") {
			x.running = append(x.running, "picsure-db")
		}
		return docker.Result{}, nil
	})
	x.f.On(fakerunner.Glob("docker compose * restart *"))
	x.f.On(fakerunner.Glob("docker compose * run --rm *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		x.migrations = true
		x.db.historyTbl, x.db.applied = 2, 7
		return docker.Result{}, nil
	})

	if err := ops.HPDSKeyStep(x.d, x.st, x.cfg).Apply(context.Background(), x.rec); err != nil {
		t.Fatal(err)
	}
	if x.state, err = x.st.LoadState(); err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *planFixture) opts() ops.ConvergeOptions {
	return ops.ConvergeOptions{Cache: x.cache, Compose: func() (docker.Composer, error) {
		x.composes++
		return docker.NewCompose(x.spy, x.st.Dir, func() []string {
			env, _ := render.ComposeEnv(x.cfg, x.sec)
			return env
		})
	}}
}

// run runs plan and returns the IDs of the steps that applied, in order.
func (x *planFixture) run(plan []steps.Step) []string {
	x.t.Helper()
	*x.rec = events.Recorder{}
	if err := steps.Run(context.Background(), x.rec, plan, steps.Options{}); err != nil {
		x.t.Fatal(err)
	}
	var applied []string
	for _, e := range x.rec.Events() {
		if done, ok := e.(events.StepDone); ok && done.Status == events.StepOK {
			applied = append(applied, done.ID)
		}
	}
	return applied
}

func (x *planFixture) up() []string {
	x.t.Helper()
	return x.run(ops.UpSteps(x.d, x.st, x.cfg, x.sec, x.state, x.opts()))
}

// callIndex is the index in the calls so far of the first that m matches,
// or -1.
func (x *planFixture) callIndex(m fakerunner.Matcher) int {
	return slices.IndexFunc(x.f.Calls(), func(c fakerunner.Call) bool { return m.Match(c.Argv) })
}

var composeWait = fakerunner.Glob("docker compose * up -d --wait *")

func TestUpOnARunningCurrentStackOnlyRendersAndStarts(t *testing.T) {
	x := newPlanFixture(t)
	// The adapter of an earlier render: render drops it, so the steps
	// after it build one over the files it wrote.
	x.d.Compose = &docker.Compose{Runner: x.f, Files: []string{"/stale/compose.yaml"}, ProjectDir: "/stale"}
	if got, want := x.up(), []string{ops.RenderStepID, ops.StartStepID}; !slices.Equal(got, want) {
		t.Errorf("applied %v, want %v", got, want)
	}
	for _, c := range x.f.Calls() {
		if strings.Contains(c.String(), "/stale") {
			t.Errorf("a step used the stale adapter: %s", c)
		}
	}
	if x.composes != 1 {
		t.Errorf("the adapter was built %d times, want once", x.composes)
	}
}

func TestUpRestartsThePendingServicesBeforeStart(t *testing.T) {
	x := newPlanFixture(t)
	x.state.PendingRestarts = []string{"psama"}
	if err := x.st.SaveState(x.state); err != nil {
		t.Fatal(err)
	}
	if got, want := x.up(), []string{ops.RenderStepID, ops.RestartStepID, ops.StartStepID}; !slices.Equal(got, want) {
		t.Errorf("applied %v, want %v", got, want)
	}
	restart, start := x.callIndex(fakerunner.Glob("docker compose * restart psama")), x.callIndex(composeWait)
	if restart < 0 || start < restart {
		t.Errorf("restart is call %d and start call %d; want the restart first", restart, start)
	}
	if state, err := x.st.LoadState(); err != nil || len(state.PendingRestarts) != 0 {
		t.Errorf("pending restarts %v (%v) after up", state.PendingRestarts, err)
	}
}

func TestUpHandsTheGatewayTheTokenSeedRenewed(t *testing.T) {
	x := newPlanFixture(t)
	x.sec.IntrospectionTokenExpiry = t0.Add(10 * 24 * time.Hour)
	if err := x.st.SaveSecrets(x.sec); err != nil {
		t.Fatal(err)
	}
	if got, want := x.up(), []string{ops.RenderStepID, ops.StepSeed, ops.StartStepID}; !slices.Equal(got, want) {
		t.Errorf("applied %v, want %v", got, want)
	}
	if x.db.token == tokenVar || x.db.token == "" {
		t.Fatalf("seed didn't renew the token: %q", x.db.token)
	}
	last := x.spy.upEnv[len(x.spy.upEnv)-1]
	if !slices.Contains(last, "PICSURE_INTROSPECTION_TOKEN="+x.db.token) {
		t.Error("compose up didn't get the renewed token")
	}
}

func TestUpAfterResetRekeysMigratesAndSeeds(t *testing.T) {
	x := newPlanFixture(t)
	// What reset leaves: nothing running, the data volumes gone, and
	// state.json's records of them cleared.
	x.running = nil
	x.hpds.createdAt = ""
	*x.db = *newFakeDB(t)
	x.db.historyTbl, x.db.applied = 0, 0
	x.migrations = false
	x.state.TLS, x.state.Truststore, x.state.HPDSKey = nil, nil, nil
	if err := x.st.SaveState(x.state); err != nil {
		t.Fatal(err)
	}
	keyRuns := x.hpds.runs
	want := []string{ops.TLSStepID, ops.RenderStepID, ops.StepDB, ops.StepMigrate, ops.StepSeed, ops.HPDSKeyStepID, ops.RestartStepID, ops.StartStepID}
	if got := x.up(); !slices.Equal(got, want) {
		t.Errorf("applied %v, want %v", got, want)
	}
	if x.hpds.runs != keyRuns+1 || x.db.token != string(x.sec.IntrospectionToken) {
		t.Errorf("key copies %d (want %d), token in the database %q", x.hpds.runs, keyRuns+1, x.db.token)
	}
}

func TestUpdateOfACurrentStackOnlyRendersAndStarts(t *testing.T) {
	x := newPlanFixture(t)
	reg := stack.Registry{Target: stack.ConfigSchema}
	rel, comps := x.target(nil)
	p := x.plan(ops.UpdateOptions{Release: rel, Components: comps, Migrations: reg})
	opts := ops.UpdateOptions{Migrations: reg, ConvergeOptions: x.opts()}
	if got, want := x.run(ops.UpdateSteps(x.d, x.st, p, x.sec, x.state, opts)), []string{ops.RenderStepID, ops.StartStepID}; !slices.Equal(got, want) {
		t.Errorf("applied %v, want %v", got, want)
	}
}

func TestInitWithHTTPDHMRTakesTheNodeTagBeforeRender(t *testing.T) {
	x := newPlanFixture(t)
	src := x.checkout(false)
	if err := os.WriteFile(filepath.Join(src, ".nvmrc"), []byte("24.11.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	x.cfg.Components.Frontend.Source = src
	x.cfg.Dev.Services = []string{"httpd-hmr"}
	x.image("hms-dbmi/pic-sure-httpd:dev-demo-"+localSHA[:12], map[string]string{
		ops.FrontendSrcLabel: localSHA, ops.FrontendConfigLabel: ops.FrontendConfigHash(render.ViteEnv(x.cfg))})
	vol := false
	x.f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_frontend-node-modules")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if !vol {
			return docker.Result{Stderr: []byte("Error response from daemon: get demo_frontend-node-modules: no such volume\n"), ExitCode: 1}, nil
		}
		out, err := json.Marshal([]map[string]any{{"Name": "demo_frontend-node-modules", "Labels": x.st.Labels("demo")}})
		return docker.Result{Stdout: out}, err
	})
	x.f.On(fakerunner.Glob("docker volume create * demo_frontend-node-modules")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		vol = true
		return docker.Result{}, nil
	})
	chown := fakerunner.Glob("docker run --rm --name demo-hmr-* alpine:* sh -c *")
	x.f.On(chown)

	got := x.run(ops.InitSteps(x.d, x.st, x.cfg, x.sec, x.state, x.opts()))
	node, rend := slices.Index(got, ops.NodeImageStepID), slices.Index(got, ops.RenderStepID)
	hmr, start := slices.Index(got, ops.HMRVolumeStepID), slices.Index(got, ops.StartStepID)
	if node < 0 || rend < node || hmr < rend || start < hmr {
		t.Errorf("applied %v, want node-image before render, and hmr-volume after it and before start", got)
	}
	compose, err := os.ReadFile(x.st.Path(render.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "node:24.11.0-alpine3.23") {
		t.Error("compose.yaml doesn't run the .nvmrc's Node")
	}
	if x.callIndex(chown) > x.callIndex(composeWait) {
		t.Error("the node_modules volume was chowned after compose up")
	}
}
