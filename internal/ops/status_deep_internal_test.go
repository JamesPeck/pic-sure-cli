package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

// A probe that ran but didn't finish within its bound is checked, with no
// verdict on the data.
func TestProbeDataTimeout(t *testing.T) {
	old := deepExecBound
	deepExecBound = func(int) time.Duration { return 100 * time.Millisecond }
	t.Cleanup(func() { deepExecBound = old })

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".pic-sure/render"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pic-sure/render/compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * exec -T hpds *")).Do(func(ctx context.Context, _ fakerunner.Call) (docker.Result, error) {
		<-ctx.Done()
		return docker.Result{ExitCode: -1}, ctx.Err()
	})
	c, err := docker.NewCompose(f, dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	dr := probeData(context.Background(), c)
	if !dr.Checked || dr.Ready != nil || !strings.Contains(dr.Message, "no answer within 100ms") {
		t.Errorf("data %+v", dr)
	}
}
