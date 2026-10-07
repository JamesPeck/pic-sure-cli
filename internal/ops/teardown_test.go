package ops_test

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// downComposer records compose down.
type downComposer struct {
	docker.Composer
	downs int
}

func (c *downComposer) Down(context.Context, docker.ComposeDownOpts) error {
	c.downs++
	return nil
}

// teardownRunner answers like fd, but filters volume ls by its label
// filters, as docker does, and removes volumes.
func teardownRunner(t *testing.T, fd *fakeDaemon) *fakerunner.Runner {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker volume ls *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		var names []string
		for name, labels := range fd.volumes {
			ok := true
			for i, a := range c.Argv {
				if a != "--filter" {
					continue
				}
				k, v, _ := strings.Cut(strings.TrimPrefix(c.Argv[i+1], "label="), "=")
				if labels[k] != v {
					ok = false
				}
			}
			if ok {
				names = append(names, name)
			}
		}
		return docker.Result{Stdout: []byte(strings.Join(names, "\n"))}, nil
	})
	f.On(fakerunner.Glob("docker volume rm *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		delete(fd.volumes, c.Argv[3])
		return docker.Result{}, nil
	})
	f.On(fakerunner.Glob("docker *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		return fd.answer(c.Argv)
	})
	return f
}

// teardownVolumes gives alpha one volume of each kind, plus volumes that
// aren't alpha's to remove.
func teardownVolumes(dir string) map[string]map[string]string {
	own := func(key string) map[string]string {
		return map[string]string{stack.LabelStack: "alpha", stack.LabelStackDir: dir, "com.docker.compose.volume": key}
	}
	return map[string]map[string]string{
		"alpha_picsure-db-data": own("picsure-db-data"),
		"alpha_hpds-data":       own("hpds-data"),
		"alpha_genomic-staging": own("genomic-staging"),
		"alpha_certs":           own("certs"),
		"alpha_truststore":      own("truststore"),
		"alpha_hpds-logs":       own("hpds-logs"),
		"alpha_custom":          own(""),
		// Labelled for alpha, but a shared data set: never the stack's.
		"demo_hpds-data": own("shared-hpds-data"),
		// Another stack named alpha, in another directory.
		"alpha2_hpds-data": {stack.LabelStack: "alpha", stack.LabelStackDir: "/elsewhere/alpha", "com.docker.compose.volume": "hpds-data"},
		// Another stack whose name merely starts with alpha's.
		"alpha_beta_hpds-data": {stack.LabelStack: "alpha_beta", stack.LabelStackDir: dir + "-beta"},
		"alpha_unlabelled":     nil,
	}
}

func openFixtureStack(t *testing.T, dir string) *stack.Stack {
	t.Helper()
	st, err := stack.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestResetRemovesOnlyTheStacksDataVolumes(t *testing.T) {
	for _, keepDB := range []bool{false, true} {
		fx := newCacheFixture(t)
		fx.daemon.volumes = teardownVolumes(fx.alpha)
		st := openFixtureStack(t, fx.alpha)
		state, _ := st.LoadState()
		state.HPDSKey = &stack.VolumeCopy{}
		if err := st.SaveState(state); err != nil {
			t.Fatal(err)
		}
		f := teardownRunner(t, fx.daemon)
		comp := &downComposer{}
		d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: comp, Clock: ops.FixedClock(cacheNow), Sink: events.Discard}

		report, err := ops.Reset(context.Background(), d, st, ops.TeardownOptions{Name: "alpha", KeepDB: keepDB})
		if err != nil {
			t.Fatal(err)
		}
		if comp.downs != 1 {
			t.Errorf("compose down ran %d times", comp.downs)
		}
		removed := []string{"alpha_certs", "alpha_genomic-staging", "alpha_hpds-data", "alpha_truststore"}
		kept := []string{"alpha_custom", "alpha_hpds-logs"}
		if keepDB {
			kept = append(kept, "alpha_picsure-db-data")
		} else {
			removed = append(removed, "alpha_picsure-db-data")
		}
		slices.Sort(removed)
		slices.Sort(kept)
		if !slices.Equal(report.Volumes, removed) {
			t.Errorf("keepDB=%v: removed %v, want %v", keepDB, report.Volumes, removed)
		}
		if !slices.Equal(report.KeptVolumes, kept) {
			t.Errorf("keepDB=%v: kept %v, want %v", keepDB, report.KeptVolumes, kept)
		}
		f.AssertCalled(fakerunner.Exact("docker", "volume", "ls", "-q",
			"--filter", "label="+stack.LabelStack+"=alpha", "--filter", "label="+stack.LabelStackDir+"="+fx.alpha))

		after, err := st.LoadState()
		if err != nil {
			t.Fatal(err)
		}
		if after.HPDSKey != nil || after.LastOperation == nil || after.LastOperation.Name != "reset" || after.LastOperation.Status != stack.OperationOK {
			t.Errorf("state after reset: hpds_key %v, last operation %+v", after.HPDSKey, after.LastOperation)
		}
		if _, err := os.Stat(st.Path(stack.ConfigFile)); err != nil {
			t.Errorf("reset touched the config: %v", err)
		}
	}
}

func TestDestroyRemovesTheStackAndNothingElse(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.volumes = teardownVolumes(fx.alpha)
	// No unreadable stack, so prune can decide.
	fx.daemon.containers = slices.DeleteFunc(fx.daemon.containers, func(c fakeContainer) bool { return c.name == "gone-hpds-1" })
	fx.daemon.images = append(fx.daemon.images, fakeImage{"hms-dbmi/pic-sure-hpds:dev-alpha_beta-aaaaaaaaaaaa", "sha256:beta-dev", 100, cacheNow.AddDate(0, 0, -2)})
	st := openFixtureStack(t, fx.alpha)
	f := teardownRunner(t, fx.daemon)
	var rec events.Recorder
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: &downComposer{}, Clock: ops.FixedClock(cacheNow), Sink: &rec}

	report, err := ops.Destroy(context.Background(), d, st, ops.TeardownOptions{Name: "alpha", PruneImages: true, Cache: fx.cache})
	if err != nil {
		t.Fatal(err)
	}
	left := slices.Sorted(maps.Keys(fx.daemon.volumes))
	if want := []string{"alpha2_hpds-data", "alpha_beta_hpds-data", "alpha_unlabelled", "demo_hpds-data"}; !slices.Equal(left, want) {
		t.Errorf("volumes left %v, want %v", left, want)
	}
	if want := []string{"hms-dbmi/pic-sure-psama:dev-alpha-aaaaaaaaaaaa"}; !slices.Equal(report.Images, want) {
		t.Errorf("dev images removed %v, want %v", report.Images, want)
	}
	if !report.Files.DirRemoved {
		t.Errorf("files: %+v, want the stack dir removed", report.Files)
	}
	var pruned []string
	for _, it := range report.Pruned.Removed {
		pruned = append(pruned, it.Name)
	}
	// alpha's state named hpds:aaaa; with alpha gone nothing uses it. The
	// gateway is in use, the visualization recent, and dev images and
	// cache entries aren't destroy's to prune.
	if want := []string{"hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa", "hms-dbmi/pic-sure-psama:cccccccccccc"}; !slices.Equal(pruned, want) {
		t.Errorf("pruned %v, want %v", pruned, want)
	}
	for _, ref := range []string{"hms-dbmi/pic-sure-gateway:bbbbbbbbbbbb", "hms-dbmi/pic-sure-psama:dev-zed-aaaaaaaaaaaa-dirty", "hms-dbmi/pic-sure-hpds:dev-alpha_beta-aaaaaaaaaaaa"} {
		if !slices.Contains(fx.daemon.refs(), ref) {
			t.Errorf("%s was removed", ref)
		}
	}
	if _, err := os.Stat(fx.cache.Root() + "/src/pic-sure/" + shaC); err != nil {
		t.Errorf("destroy pruned a source tree: %v", err)
	}
}

func TestDestroyWithoutARenderedStack(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.volumes = map[string]map[string]string{}
	st := openFixtureStack(t, fx.alpha)
	f := teardownRunner(t, fx.daemon)
	var rec events.Recorder
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Clock: ops.FixedClock(cacheNow), Sink: &rec}
	if _, err := ops.Destroy(context.Background(), d, st, ops.TeardownOptions{Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker compose *"))
	if _, err := os.Stat(fx.alpha); !os.IsNotExist(err) {
		t.Errorf("stack dir: %v, want it removed", err)
	}
}

// failingVolumeRm makes docker volume rm fail, as for a volume in use.
func failingVolumeRm(t *testing.T, fd *fakeDaemon) *fakerunner.Runner {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker volume rm *")).Stderr("Error response from daemon: remove alpha_hpds-data: volume is in use\n").Exit(1)
	inner := teardownRunner(t, fd)
	f.On(fakerunner.Glob("docker *")).Do(func(ctx context.Context, c fakerunner.Call) (docker.Result, error) {
		return inner.Run(ctx, docker.Cmd{Argv: c.Argv})
	})
	return f
}

func TestDestroyKeepsTheFilesWhenAVolumeStays(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.volumes = teardownVolumes(fx.alpha)
	st := openFixtureStack(t, fx.alpha)
	f := failingVolumeRm(t, fx.daemon)
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: &downComposer{}, Clock: ops.FixedClock(cacheNow), Sink: events.Discard}

	if _, err := ops.Destroy(context.Background(), d, st, ops.TeardownOptions{Name: "alpha"}); err == nil {
		t.Fatal("destroy succeeded with a volume it couldn't remove")
	}
	if _, err := stack.Open(fx.alpha); err != nil {
		t.Errorf("the stack no longer opens, so destroy can't be re-run: %v", err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker image rm *"))
}

func TestResetRecordsAFailureAndForgetsTheCopies(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.volumes = teardownVolumes(fx.alpha)
	st := openFixtureStack(t, fx.alpha)
	state, _ := st.LoadState()
	state.HPDSKey = &stack.VolumeCopy{}
	if err := st.SaveState(state); err != nil {
		t.Fatal(err)
	}
	f := failingVolumeRm(t, fx.daemon)
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: &downComposer{}, Clock: ops.FixedClock(cacheNow), Sink: events.Discard}

	if _, err := ops.Reset(context.Background(), d, st, ops.TeardownOptions{Name: "alpha"}); err == nil {
		t.Fatal("reset succeeded with volumes it couldn't remove")
	}
	after, err := st.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if after.HPDSKey != nil || after.LastOperation == nil || after.LastOperation.Status != stack.OperationFailed {
		t.Errorf("state after a failed reset: hpds_key %v, last operation %+v", after.HPDSKey, after.LastOperation)
	}
}
