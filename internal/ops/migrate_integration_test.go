//go:build integration

package ops_test

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

// TestMigrateAgainstDocker renders a stack from release-control's
// james_mono sources, then migrates its real databases twice: the second
// run skips both steps. It also checks the inputs, repairs, and sees access
// denied on a wrong root password. Only picsure-db, dictionary-db and the
// Flyway one-shots run. The stack's compose project is
// PICSURE_IT_PROJECT (default ws-v2-032); cleanup takes it down with its
// volumes.
//
//	go test -tags integration -run TestMigrateAgainstDocker -timeout 30m ./internal/ops/
func TestMigrateAgainstDocker(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	project := os.Getenv("PICSURE_IT_PROJECT")
	if project == "" {
		project = "ws-v2-032"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	runner := &docker.ExecRunner{}
	g := git.New(runner)
	var rec events.Recorder
	sink := events.SinkFunc(func(e events.Event) {
		rec.Emit(e)
		if l, ok := e.(events.Log); ok {
			t.Log(l.Line)
		}
	})

	root, err := cache.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(root, cache.Options{Git: g, Holder: "migrate integration test"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := stack.DefaultConfig()
	cfg.Name = project
	cfg.Auth.Mode = stack.AuthOpen
	cfg.Auth.AdminEmail = "admin@example.com"
	cfg.Release.Repo = "https://github.com/hms-dbmi/pic-sure-release-control"
	cfg.Release.Branch = "james_mono"
	// PICSURE_IT_RELEASE_COMMIT pins release-control (and so needs no fetch
	// when the cache already has the commit).
	rel, err := release.Fetch(ctx, c, g, sink, "release", release.Options{Repo: cfg.Release.Repo, Branch: cfg.Release.Branch,
		Commit: os.Getenv("PICSURE_IT_RELEASE_COMMIT")})
	if err != nil {
		t.Fatal(err)
	}
	comps, err := rel.ResolveComponents(ctx, c, sink, "release", cfg.Components)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, name := range []string{catalog.PicSure, catalog.Migrations} {
		if sources[name], err = c.EnsureSource(ctx, name, comps[name].Commit); err != nil {
			t.Fatal(err)
		}
	}

	st, err := stack.Create(filepath.Join(t.TempDir(), project))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	doc, err := stack.NewConfigDoc(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteConfig(data); err != nil {
		t.Fatal(err)
	}
	sec, err := st.EnsureSecrets(rand.Reader, stack.EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Only third-party images run here; the built ones need a tag to render.
	state := &stack.State{Images: map[string]string{}}
	for _, img := range catalog.Images() {
		if img.Built() {
			state.Images[img.Name] = "unbuilt"
		}
	}
	files, err := render.Render(render.Input{StackDir: st.Dir, Config: &cfg, State: state, Sources: sources})
	if err != nil {
		t.Fatal(err)
	}
	if err := render.Write(st, files); err != nil {
		t.Fatal(err)
	}
	env, err := render.ComposeEnv(&cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	compose, err := docker.NewCompose(runner, st.Dir, func() []string { return env })
	if err != nil {
		t.Fatal(err)
	}
	d := &ops.Deps{Runner: runner, Docker: docker.NewEngine(runner), Compose: compose, Git: g,
		Clock: ops.SystemClock{}, Rand: rand.Reader, Sink: sink}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := compose.Down(ctx, docker.ComposeDownOpts{Volumes: true}); err != nil {
			t.Errorf("compose down -v: %v", err)
		}
	})

	if r := ops.MigrateCheck(ctx, d, &cfg, sec); !r.OK {
		t.Fatalf("migrate --check: %+v", r)
	}

	stepStatus := func() map[string]events.StepStatus {
		got := map[string]events.StepStatus{}
		for _, e := range rec.Events() {
			if done, ok := e.(events.StepDone); ok {
				got[done.ID] = done.Status
			}
		}
		return got
	}
	start := time.Now()
	if err := ops.Migrate(ctx, d, &cfg, sec, ops.MigrateOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("first migrate: %s, steps %v", time.Since(start).Round(time.Second), stepStatus())
	if got := stepStatus(); got[ops.StepDB] != events.StepOK || got[ops.StepMigrate] != events.StepOK {
		t.Fatalf("first run: steps %v, want both ok", got)
	}

	rec = events.Recorder{}
	start = time.Now()
	if err := ops.Migrate(ctx, d, &cfg, sec, ops.MigrateOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("second migrate: %s, steps %v", time.Since(start).Round(time.Second), stepStatus())
	if got := stepStatus(); got[ops.StepDB] != events.StepSkipped || got[ops.StepMigrate] != events.StepSkipped {
		t.Fatalf("second run: steps %v, want both skipped", got)
	}

	rec = events.Recorder{}
	if err := ops.Migrate(ctx, d, &cfg, sec, ops.MigrateOptions{Action: ops.FlywayRepair}, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := ops.MigrationsUpToDate(ctx, d, &cfg, sec); err != nil || !ok {
		t.Fatalf("after repair: up to date %v, err %v", ok, err)
	}

	wrong := *sec
	wrong.DBRootPassword = "not-the-root-password"
	err = steps.Run(ctx, events.Discard, []steps.Step{ops.DBStep(d, &cfg, &wrong, ops.DBOptions{})}, steps.Options{})
	if exitcode.FromError(err) != exitcode.CodePrecondition {
		t.Fatalf("wrong root password: %v, want exit 3", err)
	}
	t.Logf("wrong root password: %v", err)
}
