package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

type deletingProbeEngine struct {
	docker.Engine
	volume string
}

func (e deletingProbeEngine) Run(ctx context.Context, opts docker.RunOpts) (int, error) {
	if err := e.VolumeRemove(ctx, e.volume); err != nil {
		return 0, err
	}
	opts.Labels["org.hms-dbmi.picsure.test"] = "1"
	return e.Engine.Run(ctx, opts)
}

func TestUpPreviewVolumeDeletionAgainstDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker; deletion race also covered by TestUpPreviewDoesNotRecreateDeletedVolume")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("Docker unavailable: %v", err)
	}
	engine := docker.NewEngine(&docker.ExecRunner{})
	if present, err := engine.ImageExists(ctx, "alpine:3.23"); err != nil || !present {
		t.Skipf("needs local alpine:3.23: present=%t, err=%v", present, err)
	}
	name, err := docker.UniqueName("picsure-preview-test", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	volume := name + "_hpds-genomic"
	if err := engine.VolumeCreate(ctx, volume, map[string]string{"org.hms-dbmi.picsure.test": "1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if err := engine.VolumeRemove(cleanup, volume); err != nil {
			t.Error(err)
		}
	})
	if _, err := engine.Run(ctx, docker.RunOpts{
		Image: "alpine:3.23", Remove: true, PullNever: true, Network: "none",
		Labels: map[string]string{"org.hms-dbmi.picsure.test": "1"},
		Mounts: []docker.Mount{{Source: volume, Target: "/data"}},
		Args:   []string{"mkdir", "/data/.promote-test"},
	}); err != nil {
		t.Fatalf("seed test volume: %v", err)
	}
	st, err := stack.Create(filepath.Join(t.TempDir(), "stack"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := stack.DefaultConfig()
	cfg.Name = name
	probe := &upProbeEngine{Engine: engine, report: &upPlan{}, sink: events.Discard}
	d := &ops.Deps{Docker: probe, Rand: rand.Reader}
	if got, err := ops.GenomicLeftovers(ctx, d, st, &cfg); err != nil || !slices.Equal(got, []string{".promote-test"}) {
		t.Fatalf("existing volume probe: got %v, err %v", got, err)
	}
	probe.Engine = deletingProbeEngine{Engine: engine, volume: volume}
	if _, err := ops.GenomicLeftovers(ctx, d, st, &cfg); err == nil {
		t.Error("probe of concurrently deleted volume succeeded")
	}
	if _, err := engine.VolumeInspect(ctx, volume); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("volume exists after deletion and probe: %v", err)
	}
}
