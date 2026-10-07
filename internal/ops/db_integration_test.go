//go:build integration

package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	"github.com/JamesPeck/pic-sure-cli/internal/sql"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// TestBootstrapAgainstDocker runs a mysql:8.0 container as the "remote"
// database, published on a random host port and reached as
// host.docker.internal (Docker Desktop, OrbStack), and renders a remote-mode
// stack from release-control's james_mono sources. It checks and
// bootstraps the remote database, migrates it, then changes a password in
// the secrets and syncs it with --sync-passwords. The compose project is
// PICSURE_IT_PROJECT (default ws-v2-054), and the remote container is named
// v2-054-remote-<rand>. Cleanup takes the project down with its volumes and
// removes the container, unless PICSURE_IT_KEEP is set: then both stay, and
// the test logs the stack directory and the container's name.
//
//	go test -tags integration -run TestBootstrapAgainstDocker -timeout 30m ./internal/ops/
func TestBootstrapAgainstDocker(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	project := os.Getenv("PICSURE_IT_PROJECT")
	if project == "" {
		project = "ws-v2-054"
	}
	keep := os.Getenv("PICSURE_IT_KEEP") != ""
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

	// The remote server.
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	remote := "v2-054-remote-" + hex.EncodeToString(suffix)
	rootPassword := "Remote-root-" + hex.EncodeToString(suffix)
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--name", remote, "-e", "MYSQL_ROOT_PASSWORD",
		"-p", "127.0.0.1::3306", "mysql:8.0")
	run.Env = append(os.Environ(), "MYSQL_ROOT_PASSWORD="+rootPassword)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("starting %s: %v\n%s", remote, err, out)
	}
	if !keep {
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", remote).Run() })
	}
	out, err := exec.Command("docker", "port", remote, "3306/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := strings.Cut(strings.TrimSpace(strings.Split(string(out), "\n")[0]), ":")
	t.Logf("remote %s on host port %s", remote, port)

	root, err := cache.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(root, cache.Options{Git: g, Holder: "bootstrap integration test"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := stack.DefaultConfig()
	cfg.Name = project
	cfg.Auth.Mode = stack.AuthOpen
	cfg.Auth.AdminEmail = "admin@example.com"
	cfg.Release.Repo = "https://github.com/hms-dbmi/pic-sure-release-control"
	cfg.Release.Branch = "james_mono"
	cfg.DB.Mode = stack.DBRemote
	cfg.DB.Remote = stack.RemoteDB{Host: "host.docker.internal", RootUser: "root"}
	if cfg.DB.Remote.Port, err = strconv.Atoi(port); err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
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

	dir := filepath.Join(t.TempDir(), project)
	if keep {
		if dir, err = os.MkdirTemp("", project+"-"); err != nil {
			t.Fatal(err)
		}
	}
	st, err := stack.Create(dir)
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
	sec, err := st.EnsureSecrets(rand.Reader, stack.EnsureOptions{RemoteDB: true,
		Supplied: stack.UserSecrets{DBRemoteRootPassword: stack.Secret(rootPassword)}})
	if err != nil {
		t.Fatal(err)
	}
	state := &stack.State{CLIVersion: "dev", SchemaVersion: 1, Images: map[string]string{}}
	for _, img := range catalog.Images() {
		if img.Built() {
			state.Images[img.Name] = "unbuilt"
		}
	}
	if err := st.SaveState(state); err != nil {
		t.Fatal(err)
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
	if keep {
		t.Logf("kept: stack %s, remote container %s", st.Dir, remote)
	} else {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := compose.Down(ctx, docker.ComposeDownOpts{Volumes: true}); err != nil {
				t.Errorf("compose down -v: %v", err)
			}
		})
	}

	// mysqld restarts once while it initialises; wait for the real server.
	rootTarget := sql.MySQLTarget{Host: cfg.DB.Remote.Host, Port: cfg.DB.Remote.Port, Password: rootPassword}
	for deadline := time.Now().Add(3 * time.Minute); ; {
		if rows, err := sql.QueryMySQL(ctx, d.Docker, rootTarget, "SELECT COUNT(*) FROM mysql.user WHERE User = 'root'"); err == nil && len(rows) == 1 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the remote server isn't ready: %v", err)
		}
		time.Sleep(2 * time.Second)
	}

	r, err := ops.CheckBootstrap(ctx, d, &cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("check before bootstrap: ok %v, version %s, failing %v", r.OK, r.Version, failing(r))
	if r.OK {
		t.Fatal("an empty server checked ok")
	}
	if err := ops.Bootstrap(ctx, d, &cfg, sec, ops.BootstrapOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if r, err = ops.CheckBootstrap(ctx, d, &cfg, sec); err != nil || !r.OK {
		t.Fatalf("check after bootstrap: %+v, %v", r, err)
	}
	t.Logf("check after bootstrap: ok, %d checks", len(r.Checks))

	rec = events.Recorder{}
	start := time.Now()
	if err := ops.Migrate(ctx, d, &cfg, sec, ops.MigrateOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("migrate: %s, steps %v", time.Since(start).Round(time.Second), stepStatuses(&rec))
	if got := stepStatuses(&rec); got[ops.StepDBBootstrap] != events.StepSkipped || got[ops.StepMigrate] != events.StepOK {
		t.Fatalf("migrate steps %v", got)
	}
	if ok, err := ops.MigrationsUpToDate(ctx, d, &cfg, sec); err != nil || !ok {
		t.Fatalf("after migrate: up to date %v, err %v", ok, err)
	}

	// A new password in the secrets: bootstrap refuses until it syncs.
	changed := *sec
	changed.DBPicsurePassword = stack.Secret("Changed-picsure-" + hex.EncodeToString(suffix))
	err = ops.Bootstrap(ctx, d, &cfg, &changed, ops.BootstrapOptions{}, nil)
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "--sync-passwords") {
		t.Fatalf("bootstrap with a changed password: %v, want exit 3 suggesting --sync-passwords", err)
	}
	t.Logf("changed password without sync: %v", err)
	if err := ops.Bootstrap(ctx, d, &cfg, &changed, ops.BootstrapOptions{SyncPasswords: true}, nil); err != nil {
		t.Fatal(err)
	}
	login := sql.MySQLTarget{Host: cfg.DB.Remote.Host, Port: cfg.DB.Remote.Port, User: "picsure",
		Password: string(changed.DBPicsurePassword), Database: "picsure"}
	rows, err := sql.QueryMySQL(ctx, d.Docker, login, "SELECT COUNT(*) FROM flyway_schema_history")
	if err != nil {
		t.Fatalf("picsure can't log in with its new password: %v", err)
	}
	t.Logf("picsure logs in with its new password; picsure.flyway_schema_history has %s rows", rows[0][0])
	login.Password = string(sec.DBPicsurePassword)
	if err := sql.ExecMySQL(ctx, d.Docker, login, "SELECT 1"); err == nil {
		t.Fatal("picsure still logs in with its old password")
	}
}
