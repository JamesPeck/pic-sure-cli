package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func TestUpPreviewRejectsUnsafeProbe(t *testing.T) {
	for _, opts := range []docker.RunOpts{
		{Remove: true, Network: "bridge"},
		{Network: "none"},
		{Remove: true, Network: "none", Mounts: []docker.Mount{{Source: "volume", Target: "/data"}}},
	} {
		opts.Name = "demo-genomic-list-rejected"
		engine := &upProbeEngine{report: &upPlan{}, sink: events.Discard}
		if _, err := engine.Run(context.Background(), opts); err == nil {
			t.Fatal("unsafe probe reached Docker")
		}
		if err := engine.Rm(context.Background(), opts.Name, true); err != nil {
			t.Fatalf("rejected probe cleanup: %v", err)
		}
	}
}

func TestUpPreviewRejectsUnsupportedVolumeMounts(t *testing.T) {
	for _, volume := range []string{
		`{"Driver":"plugin","Mountpoint":"/data"}`,
		`{"Driver":"local","Mountpoint":"/data","Options":{"type":"nfs"}}`,
		`{"Driver":"local"}`,
	} {
		t.Run(volume, func(t *testing.T) {
			f := fakerunner.New(t)
			f.On(fakerunner.Exact("docker", "volume", "inspect", "demo_hpds-genomic")).Stdout("[" + volume + "]")
			engine := &upProbeEngine{Engine: docker.NewEngine(f), report: &upPlan{}, sink: events.Discard}
			_, err := engine.Run(context.Background(), docker.RunOpts{
				Name: "probe", Image: "alpine:3.23", Remove: true, Network: "none",
				Mounts: []docker.Mount{{Source: "demo_hpds-genomic", Target: "/data", ReadOnly: true}},
			})
			if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "cannot safely probe") {
				t.Fatalf("unsupported volume: %v", err)
			}
			if err := engine.Rm(context.Background(), "probe", true); err != nil {
				t.Fatal(err)
			}
			f.AssertNotCalled(fakerunner.Glob("docker run *"))
			f.AssertNotCalled(fakerunner.Glob("docker rm *"))
		})
	}
}

func TestUpPreviewDoesNotRecreateDeletedVolume(t *testing.T) {
	ctx := context.Background()
	const volume = "demo_hpds-genomic"
	source := filepath.Join(t.TempDir(), "_data")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	present := true
	f := fakerunner.New(t)
	f.On(fakerunner.Exact("docker", "volume", "inspect", volume)).
		Stdout(`[{"Name":"` + volume + `","Driver":"local","Mountpoint":"` + source + `"}]`)
	f.On(fakerunner.Glob("docker run *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		// Reset wins after the last inspect, immediately before Docker mounts.
		present = false
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(c.Argv, volume+":/data:ro") {
			present = true // Docker -v creates an absent named volume, even :ro.
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			return docker.Result{}, nil
		}
		want := "type=bind,source=" + source + ",target=/data,readonly"
		if !slices.Contains(c.Argv, "--mount") || !slices.Contains(c.Argv, want) {
			t.Fatalf("probe must use an existing bind source: %v", c.Argv)
		}
		return docker.Result{ExitCode: 125, Stderr: []byte("invalid mount config for type bind: bind source path does not exist")}, nil
	})
	f.On(fakerunner.Glob("docker rm *"))
	engine := &upProbeEngine{Engine: docker.NewEngine(f), report: &upPlan{}, sink: events.Discard}
	d := &ops.Deps{Docker: engine, Rand: bytes.NewReader(make([]byte, 4))}
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	st, err := stack.Create(filepath.Join(t.TempDir(), "stack"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, err = ops.GenomicLeftovers(ctx, d, st, &cfg)
	if present {
		t.Error("preview recreated the deleted volume")
	}
	if _, statErr := os.Stat(source); !os.IsNotExist(statErr) {
		t.Errorf("deleted volume directory exists afterwards: %v", statErr)
	}
	if err == nil {
		t.Error("preview must report a failed Check when its source disappears")
	}
	f.AssertCalled(fakerunner.Glob("docker run *"))
}
