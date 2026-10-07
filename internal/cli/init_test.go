package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
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
	r := &initRun{a: a, cmd: cmd, dir: dir, d: a.newDeps()}
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
	r2, err := newInitRun(t, r.dir, "", "--name", "other")
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
	if r2.state.Release.Commit != r.rel.Commit {
		t.Errorf("resume replaced the recorded release with %s", r2.state.Release.Commit)
	}
}
