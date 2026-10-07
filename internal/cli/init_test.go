package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/release"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// envRunner records the environment of each command it is given.
type envRunner struct{ env [][]string }

func (r *envRunner) Run(_ context.Context, c docker.Cmd) (docker.Result, error) {
	r.env = append(r.env, c.Env)
	return docker.Result{}, nil
}

func (r *envRunner) Stream(_ context.Context, c docker.Cmd, _, _ io.Writer) (int, error) {
	r.env = append(r.env, c.Env)
	return 0, nil
}

// The seed step issues the introspection token into the Secrets init
// holds, after the Composer was built; compose up must still hand it to
// the gateway.
func TestInitComposeSeesTheTokenSeedIssued(t *testing.T) {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := os.MkdirAll(st.Path(".pic-sure/render"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.Path(".pic-sure/render/compose.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _, _ := testApp(t)
	cfg := stack.DefaultConfig()
	rec := &envRunner{}
	r := &initRun{a: a, d: &ops.Deps{Runner: rec}, st: st, cfg: &cfg, sec: &stack.Secrets{}}
	c, err := r.compose()
	if err != nil {
		t.Fatal(err)
	}
	r.sec.IntrospectionToken = "synthetic-token-from-seed"
	if err := c.Up(context.Background(), docker.ComposeUpOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.env) != 1 || !slices.Contains(rec.env[0], "PICSURE_INTROSPECTION_TOKEN=synthetic-token-from-seed") {
		t.Errorf("compose up didn't get the token seed issued")
	}
}

// newInitRun parses init's command line for a stack in dir (a new one
// when ""), as initStack would, up to its config and secrets.
func newInitRun(t *testing.T, dir, stdin string, args ...string) (*initRun, error) {
	t.Helper()
	a, _, _ := testApp(t)
	a.Stdin = strings.NewReader(stdin)
	cmd := newInitCmd(a)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	cmd.SetContext(context.Background())
	if dir == "" {
		tmp, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dir = filepath.Join(tmp, "demo")
	}
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &initRun{a: a, cmd: cmd, dir: dir, d: a.newDeps(), cache: c, host: anyPortFree{}}
	t.Cleanup(func() {
		if r.lock != nil {
			_ = r.lock.Unlock()
		}
		if r.st != nil {
			_ = r.st.Close()
		}
	})
	if err := r.readConfig(); err != nil {
		return r, err
	}
	return r, r.readSecrets()
}

func TestInitReadsOneSecretPerLineFromStdin(t *testing.T) {
	r, err := newInitRun(t, "", "synthetic-client-secret-0123456789abcdef\r\nsynthetic-root-password\n",
		"--name", "demo", "--admin-email", "admin@example.com", "--auth0-client-id", "abc",
		"--db-mode", "remote", "--db-host", "db.example.com",
		"--db-root-password-stdin", "--auth0-client-secret-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if r.supplied.Auth0ClientSecret != "synthetic-client-secret-0123456789abcdef" || r.supplied.DBRemoteRootPassword != "synthetic-root-password" {
		t.Errorf("secrets routed wrongly: client secret %d bytes, root password %d bytes",
			len(r.supplied.Auth0ClientSecret), len(r.supplied.DBRemoteRootPassword))
	}
}

func TestInitWritesTheConfigSecretsAndState(t *testing.T) {
	r, err := newInitRun(t, "", "", "--name", "demo", "--admin-email", "admin@example.com", "--auth-mode", "open",
		"--http-port", "8083", "--https-port", "8443", "--release-branch", "james_mono")
	if err != nil {
		t.Fatal(err)
	}
	r.rel = &release.Release{Repo: "https://example.com/release-control", Branch: "james_mono", Commit: strings.Repeat("a", 40)}
	if err := r.writeConfig(context.Background(), r.d.Sink); err != nil {
		t.Fatal(err)
	}
	cfg, err := r.st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "demo" || cfg.Network.HTTPSPort != 8443 || cfg.Release.Branch != "james_mono" || cfg.Network.DevPorts.Base != 15000 {
		t.Errorf("pic-sure.yaml: %+v", cfg.Network)
	}
	if !r.sec.Auth0ClientSecretGenerated || len(r.sec.Auth0ClientSecret) != 64 {
		t.Error("open mode without a client secret didn't get a generated one")
	}
	state, err := r.st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Release.Commit != r.rel.Commit || state.CLIVersion != r.a.Info.Version || state.SchemaVersion != stack.ConfigSchema ||
		state.LastOperation == nil || state.LastOperation.Name != "init" {
		t.Errorf("state.json: %+v", state)
	}

	// A resumed run keeps the config as it is and the recorded release.
	data, err := os.ReadFile(r.st.Path(stack.ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newInitRun(t, r.dir, "", "--name", "other"); exitcode.FromError(err) != exitcode.CodeUsage {
		t.Errorf("resume with another --name: %v, want a usage error", err)
	}
	// The same flags resume, and a client secret given now replaces the
	// generated one.
	r2, err := newInitRun(t, r.dir, "synthetic-client-secret-0123456789abcdef\n", "--name", "demo", "--auth-mode", "open",
		"--http-port", "8083", "--auto-ports", "--auth0-client-secret-stdin")
	if err != nil || !r2.resumed || r2.cfg.Name != "demo" {
		t.Fatalf("resume: err %v, resumed %v, name %s", err, r2.resumed, r2.cfg.Name)
	}
	if r2.prior, err = ops.PeekState(r.dir); err != nil {
		t.Fatal(err)
	}
	if opts := r2.releaseOptions(); opts.Commit != r.rel.Commit || opts.Repo != r.rel.Repo {
		t.Errorf("resume fetches %+v, want the recorded release", opts)
	}
	_ = r.lock.Unlock()
	r.lock = nil
	r2.rel = &release.Release{Commit: strings.Repeat("b", 40)}
	if err := r2.writeConfig(context.Background(), r2.d.Sink); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(r.st.Path(stack.ConfigFile))
	if err != nil || string(after) != string(data) {
		t.Errorf("resume rewrote pic-sure.yaml (err %v)", err)
	}
	if r2.sec.Auth0ClientSecretGenerated || r2.sec.Auth0ClientSecret != "synthetic-client-secret-0123456789abcdef" {
		t.Error("resume didn't replace the generated client secret with the one given")
	}
	if r2.state.Release.Commit != r.rel.Commit {
		t.Errorf("resume replaced the recorded release with %s", r2.state.Release.Commit)
	}
}

func TestInitWritesTheSetValues(t *testing.T) {
	r, err := newInitRun(t, "", "", "--name", "demo", "--admin-email", "admin@example.com", "--auth-mode", "open",
		"--set", "hpds.java_opts=-Xmx2g", "--set", "network.dev_ports.base=16000", "--set", "dev.services=hpds,psama")
	if err != nil {
		t.Fatal(err)
	}
	r.rel = &release.Release{Repo: "https://example.com/release-control", Branch: "main", Commit: strings.Repeat("a", 40)}
	if err := r.writeConfig(context.Background(), r.d.Sink); err != nil {
		t.Fatal(err)
	}
	cfg, err := r.st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HPDS.JavaOpts != "-Xmx2g" || cfg.Network.DevPorts.Base != 16000 || !slices.Equal(cfg.Dev.Services, []string{"hpds", "psama"}) {
		t.Errorf("pic-sure.yaml: hpds %+v, network %+v, dev %+v", cfg.HPDS, cfg.Network, cfg.Dev)
	}
}

// Inits writing their config at once share one cache, so each sees the
// ports the others claimed and chooses different ones.
func TestConcurrentInitsChooseDifferentPorts(t *testing.T) {
	const n = 4
	root := filepath.Join(t.TempDir(), "cache")
	runs := make([]*initRun, n)
	for i := range runs {
		r, err := newInitRun(t, "", "", "--name", fmt.Sprintf("demo%d", i), "--admin-email", "admin@example.com",
			"--auth-mode", "open", "--auto-ports")
		if err != nil {
			t.Fatal(err)
		}
		if r.cache, err = cache.Open(root, cache.Options{}); err != nil {
			t.Fatal(err)
		}
		r.rel = &release.Release{Repo: "https://example.com/release-control", Branch: "main", Commit: strings.Repeat("a", 40)}
		runs[i] = r
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, r := range runs {
		wg.Go(func() { errs[i] = r.writeConfig(context.Background(), r.d.Sink) })
	}
	wg.Wait()
	seen := map[int]string{}
	for i, r := range runs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		cfg, err := r.st.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		net := cfg.Network
		ports := []int{net.HTTPPort, net.HTTPSPort}
		for p := net.DevPorts.Base; p < net.DevPorts.Base+catalog.DevPortSpan; p++ {
			ports = append(ports, p)
		}
		for _, p := range ports {
			if other, ok := seen[p]; ok {
				t.Errorf("%s and %s both have port %d", other, cfg.Name, p)
			}
			seen[p] = cfg.Name
		}
	}
}

// Ports the user gave are kept even when another stack has them, and only
// ports init chose itself are chosen again when one is taken at start.
func TestInitKeepsGivenPorts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	rel := &release.Release{Repo: "https://example.com/release-control", Branch: "main", Commit: strings.Repeat("a", 40)}
	start := func(args ...string) *initRun {
		t.Helper()
		r, err := newInitRun(t, "", "", append([]string{"--admin-email", "admin@example.com", "--auth-mode", "open"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		if r.cache, err = cache.Open(root, cache.Options{}); err != nil {
			t.Fatal(err)
		}
		r.rel = rel
		if err := r.writeConfig(context.Background(), r.d.Sink); err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := start("--name", "a", "--auto-ports")
	if a.cfg.Network.HTTPPort != 8080 || a.cfg.Network.HTTPSPort != 8443 || a.cfg.Network.DevPorts.Base != 15000 {
		t.Fatalf("a: %+v", a.cfg.Network)
	}
	b := start("--name", "b", "--http-port", "8080", "--auto-ports")
	if b.cfg.Network.HTTPPort != 8080 || b.cfg.Network.HTTPSPort != 8444 || b.cfg.Network.DevPorts.Base != 15010 {
		t.Fatalf("b: %+v", b.cfg.Network)
	}

	taken := func(port int) error {
		return fmt.Errorf("start: %w", &docker.ExitError{Argv: []string{"docker", "compose", "up"}, ExitCode: 1,
			Stderr: fmt.Appendf(nil, "Error response from daemon: Bind for 0.0.0.0:%d failed: port is already allocated\n", port)})
	}
	for _, tc := range []struct {
		port  int
		retry bool
	}{{8080, false}, {8444, true}, {15013, true}, {15000, false}, {9999, false}} {
		if _, choose := b.portRetry(taken(tc.port)); (choose != nil) != tc.retry {
			t.Errorf("port %d taken: retry %v, want %v", tc.port, choose != nil, tc.retry)
		}
	}
	if _, choose := b.portRetry(errors.New("compose up failed")); choose != nil {
		t.Error("retry on an error that isn't a taken port")
	}
	// Without --auto-ports init chose 80 and 443, and has no others.
	c := start("--name", "c")
	if _, choose := c.portRetry(taken(80)); choose != nil {
		t.Error("retry on the default port 80 without --auto-ports")
	}

	// A taken dev port moves only the dev block.
	port, choose := b.portRetry(taken(15013))
	if err := b.claimPorts(context.Background(), b.d.Sink, "", choose, port); err != nil {
		t.Fatal(err)
	}
	if net := b.cfg.Network; net.HTTPPort != 8080 || net.HTTPSPort != 8444 || net.DevPorts.Base != 15030 {
		t.Errorf("after dev port 15013 was taken: %+v, want 8080/8444 and base 15030", net)
	}
	// A taken HTTPS port moves every port init chose, and pic-sure.yaml
	// has them.
	port, choose = b.portRetry(taken(8444))
	if err := b.claimPorts(context.Background(), b.d.Sink, "", choose, port); err != nil {
		t.Fatal(err)
	}
	cfg, err := b.st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if net := cfg.Network; net.HTTPPort != 8080 || net.HTTPSPort != 8445 || net.DevPorts.Base != 15010 {
		t.Errorf("after 8444 was taken, pic-sure.yaml has %+v, want 8080/8445 and base 15010", net)
	}
}

// The retry runs init's plan from render, with the --skip-step values
// that name its steps.
func TestInitRetryPlanStartsAtRender(t *testing.T) {
	r, err := newInitRun(t, "", "", "--name", "demo", "--admin-email", "admin@example.com", "--auth-mode", "open")
	if err != nil {
		t.Fatal(err)
	}
	r.a.Global.SkipSteps = []string{"images", ops.StartStepID}
	r.sec, r.state = &stack.Secrets{}, &stack.State{}
	plan, skip := r.retryPlan(ops.ConvergeOptions{})
	if plan[0].ID != ops.RenderStepID || plan[len(plan)-1].ID != ops.StartStepID {
		t.Errorf("plan runs %s to %s", plan[0].ID, plan[len(plan)-1].ID)
	}
	if !slices.Equal(skip, []string{ops.StartStepID}) {
		t.Errorf("skip = %v", skip)
	}
}
