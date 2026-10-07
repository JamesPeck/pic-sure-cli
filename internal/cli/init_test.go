package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
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
