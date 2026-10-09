package ops_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func TestHMRVolumeStepRemovesAnInterruptedHelper(t *testing.T) {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	cfg.Components.Frontend.Source = t.TempDir()
	f := fakerunner.New(t)
	f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_frontend-node-modules")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		out, err := json.Marshal([]map[string]any{{"Name": "demo_frontend-node-modules", "Labels": st.Labels("demo")}})
		return docker.Result{Stdout: out}, err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.On(fakerunner.Glob("docker run --rm --name demo-hmr-* alpine:* sh -c *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		cancel()
		return docker.Result{}, context.Canceled
	})
	rm := fakerunner.Glob("docker rm -v -f demo-hmr-*")
	f.On(rm)
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Rand: rand.Reader, Sink: &events.Recorder{}}
	if err := ops.HMRVolumeStep(d, st, &cfg, "501:20").Apply(ctx, d.Sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply = %v, want it interrupted", err)
	}
	f.AssertCalled(rm)
}
