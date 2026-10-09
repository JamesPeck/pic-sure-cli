package ops_test

import (
	"context"
	"maps"
	"os/exec"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// testLabel marks every image, volume and container the Docker tests
// create, so that `make clean-test-docker` can remove what a killed run
// leaves behind. Keep the two in step.
const testLabel = "org.hms-dbmi.picsure.test"

func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs docker")
	}
	// A wedged daemon can accept the connection and never answer.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
}

// labelEngine is the real engine, adding testLabel to everything it
// builds or creates.
type labelEngine struct{ docker.Engine }

func newLabelEngine() docker.Engine {
	return labelEngine{docker.NewEngine(&docker.ExecRunner{})}
}

func withTestLabel(labels map[string]string) map[string]string {
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	out[testLabel] = "1"
	return out
}

func (e labelEngine) Build(ctx context.Context, opts docker.BuildOpts) error {
	opts.Labels = withTestLabel(opts.Labels)
	return e.Engine.Build(ctx, opts)
}

func (e labelEngine) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	return e.Engine.VolumeCreate(ctx, name, withTestLabel(labels))
}

func (e labelEngine) Run(ctx context.Context, opts docker.RunOpts) (int, error) {
	opts.Labels = withTestLabel(opts.Labels)
	return e.Engine.Run(ctx, opts)
}

func (e labelEngine) Create(ctx context.Context, opts docker.RunOpts) (string, error) {
	opts.Labels = withTestLabel(opts.Labels)
	return e.Engine.Create(ctx, opts)
}

// cleanupDocker registers f to run when the test ends, on its own context
// bounded at two minutes rather than the test's, which may be cancelled.
func cleanupDocker(t *testing.T, f func(ctx context.Context)) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		f(ctx)
	})
}
