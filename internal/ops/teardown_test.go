package ops_test

import (
	"context"
	"encoding/json"
	"maps"
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

// downComposer records compose down.
type downComposer struct {
	docker.Composer
	downs int
}

func (c *downComposer) Down(context.Context, docker.ComposeDownOpts) error {
	c.downs++
	return nil
}

// teardownRunner answers like fd, but filters volume, network and
// container lists by their label filters, as docker does, and removes
// volumes.
func teardownRunner(t *testing.T, fd *fakeDaemon) *fakerunner.Runner {
	f := fakerunner.New(t)
	matching := func(argv []string, all map[string]map[string]string) []string {
		var names []string
		for name, labels := range all {
			ok := true
			for i, a := range argv {
				if a != "--filter" {
					continue
				}
				k, v, _ := strings.Cut(strings.TrimPrefix(argv[i+1], "label="), "=")
				if labels[k] != v {
					ok = false
				}
			}
			if ok {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		return names
	}
	f.On(fakerunner.Glob("docker volume ls *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stdout: []byte(strings.Join(matching(c.Argv, fd.volumes), "\n"))}, nil
	})
	f.On(fakerunner.Glob("docker network ls *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		return docker.Result{Stdout: []byte(strings.Join(matching(c.Argv, fd.networks), "\n"))}, nil
	})
	f.On(fakerunner.Glob("docker ps --all --no-trunc *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		containers := map[string]map[string]string{}
		for _, ct := range fd.containers {
			containers[ct.name] = ct.labels
		}
		var out strings.Builder
		for _, name := range matching(c.Argv, containers) {
			b, err := json.Marshal(map[string]any{"Names": name, "Ports": "", "Labels": containers[name]})
			if err != nil {
				return docker.Result{}, err
			}
			out.Write(append(b, '\n'))
		}
		return docker.Result{Stdout: []byte(out.String())}, nil
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
	if err := fx.cache.RegisterStack(context.Background(), fx.alpha, "alpha"); err != nil {
		t.Fatal(err)
	}
	f := teardownRunner(t, fx.daemon)
	var rec events.Recorder
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: &downComposer{}, Clock: ops.FixedClock(cacheNow), Sink: &rec}

	report, err := ops.Destroy(context.Background(), d, st, ops.TeardownOptions{Name: "alpha", PruneImages: true, Cache: fx.cache})
	if err != nil {
		t.Fatal(err)
	}
	left := slices.Sorted(maps.Keys(fx.daemon.volumes))
	if want := []string{"alpha_beta_hpds-data", "alpha_unlabelled", "demo_hpds-data"}; !slices.Equal(left, want) {
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
	if reg, err := fx.cache.RegisteredStacks(); err != nil || len(reg) > 0 {
		t.Errorf("registry after destroy: %+v, %v; want it empty", reg, err)
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

// copyFixture is alpha, with a stack ID and its volumes and container
// labelled with it, and a copy of its directory.
type copyFixture struct {
	fx         *cacheFixture
	st, copied *stack.Stack
	id         string
}

func newCopyFixture(t *testing.T) *copyFixture {
	fx := newCacheFixture(t)
	st := openFixtureStack(t, fx.alpha)
	id, err := st.EnsureID(strings.NewReader(strings.Repeat("a", 16)))
	if err != nil {
		t.Fatal(err)
	}
	labels := func(key string) map[string]string {
		l := st.Labels("alpha")
		l["com.docker.compose.project"], l["com.docker.compose.volume"] = "alpha", key
		return l
	}
	fx.daemon.volumes = map[string]map[string]string{"alpha_hpds-data": labels("hpds-data"), "alpha_picsure-db-data": labels("picsure-db-data")}
	fx.daemon.containers = []fakeContainer{{name: "alpha-hpds-1", labels: labels("")}}
	dir := filepath.Join(filepath.Dir(fx.alpha), "alpha-copy")
	if err := os.CopyFS(dir, os.DirFS(fx.alpha)); err != nil {
		t.Fatal(err)
	}
	return &copyFixture{fx: fx, st: st, copied: openFixtureStack(t, dir), id: id}
}

func TestDestroyInACopyRemovesOnlyItsFiles(t *testing.T) {
	cx := newCopyFixture(t)
	f := teardownRunner(t, cx.fx.daemon)
	comp := &downComposer{}
	var rec events.Recorder
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: comp, Clock: ops.FixedClock(cacheNow), Sink: &rec}

	// A volume of the copy's own is left too, and named.
	own := cx.copied.Labels("alpha")
	own["com.docker.compose.project"] = "alpha"
	cx.fx.daemon.volumes["alpha_copy-only"] = own

	report, err := ops.Destroy(context.Background(), d, cx.copied, ops.TeardownOptions{Name: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if comp.downs != 0 {
		t.Error("compose down ran in the copy")
	}
	if !slices.ContainsFunc(rec.Events(), func(e events.Event) bool {
		w, ok := e.(events.Warning)
		return ok && strings.Contains(w.Text, "this stack's own Docker resources are left too") && strings.Contains(w.Text, "volume alpha_copy-only")
	}) {
		t.Errorf("no warning naming the copy's own volume in %v", rec.Events())
	}
	f.AssertNotCalled(fakerunner.Glob("docker volume rm *"))
	f.AssertNotCalled(fakerunner.Glob("docker image rm *"))
	if len(cx.fx.daemon.volumes) != 3 {
		t.Errorf("volumes left %v, want all three", slices.Sorted(maps.Keys(cx.fx.daemon.volumes)))
	}
	if len(report.LeftAlone) != 3 || report.LeftAlone[0].StackDir != cx.st.Dir {
		t.Errorf("left alone %+v, want alpha's container and two volumes", report.LeftAlone)
	}
	if !report.Files.DirRemoved {
		t.Errorf("files: %+v, want the copy removed", report.Files)
	}
	if _, err := os.Stat(cx.st.Path(stack.ConfigFile)); err != nil {
		t.Errorf("the original's config: %v", err)
	}
}

func TestResetInACopyRefuses(t *testing.T) {
	cx := newCopyFixture(t)
	f := teardownRunner(t, cx.fx.daemon)
	comp := &downComposer{}
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: comp, Clock: ops.FixedClock(cacheNow), Sink: events.Discard}

	_, err := ops.Reset(context.Background(), d, cx.copied, ops.TeardownOptions{Name: "alpha"})
	if exitcode.FromError(err) != exitcode.CodePrecondition || !strings.Contains(err.Error(), "  stack alpha in "+cx.st.Dir+": container alpha-hpds-1, volume alpha_hpds-data, volume alpha_picsure-db-data") {
		t.Fatalf("err = %v, want exit 3 naming alpha's volume and directory", err)
	}
	if comp.downs != 0 || len(cx.fx.daemon.volumes) != 2 {
		t.Errorf("reset in a copy ran compose down %d times, left volumes %v", comp.downs, cx.fx.daemon.volumes)
	}
}

func TestDestroyAfterAMoveAdoptsTheResources(t *testing.T) {
	cx := newCopyFixture(t)
	// The original is gone: the copy is the stack, moved.
	if err := os.RemoveAll(cx.st.Dir); err != nil {
		t.Fatal(err)
	}
	f := teardownRunner(t, cx.fx.daemon)
	comp := &downComposer{}
	var rec events.Recorder
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: comp, Clock: ops.FixedClock(cacheNow), Sink: &rec}

	report, err := ops.Destroy(context.Background(), d, cx.copied, ops.TeardownOptions{Name: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if comp.downs != 1 || len(cx.fx.daemon.volumes) != 0 || len(report.LeftAlone) != 0 {
		t.Errorf("compose down ran %d times, volumes left %v, left alone %v; want the moved stack's all removed", comp.downs, cx.fx.daemon.volumes, report.LeftAlone)
	}
	if !slices.ContainsFunc(rec.Events(), func(e events.Event) bool {
		w, ok := e.(events.Warning)
		return ok && w.Text == "stack moved from "+cx.st.Dir+"; adopting its resources"
	}) {
		t.Errorf("no note of the move in %v", rec.Events())
	}
}

func TestResetRemovesLeftHelperContainersFirst(t *testing.T) {
	cx := newCopyFixture(t)
	// The original's leaked dictionary-etl, a helper of another stack
	// named alpha (the copy's), and one compose started.
	etl := cx.st.Labels("alpha")
	cx.fx.daemon.containers = []fakeContainer{
		{name: "alpha-dictionaryetl-0a1b2c3d", labels: etl},
		{name: "alpha-hpds-load-11223344", labels: cx.copied.Labels("alpha")},
		{name: "alpha-hpds-1", labels: withLabel(etl, "com.docker.compose.project", "alpha")},
	}
	f := teardownRunner(t, cx.fx.daemon)
	comp := &downComposer{}
	var rec events.Recorder
	d := &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Compose: comp, Clock: ops.FixedClock(cacheNow), Sink: &rec}

	if _, err := ops.Reset(context.Background(), d, cx.st, ops.TeardownOptions{Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if got := f.CallsMatching(fakerunner.Glob("docker rm *")); len(got) != 1 || got[0].Argv[4] != "alpha-dictionaryetl-0a1b2c3d" {
		t.Errorf("removed %v, want only alpha's dictionary-etl", got)
	}
	if !slices.ContainsFunc(rec.Events(), func(e events.Event) bool {
		w, ok := e.(events.Warning)
		return ok && w.Text == "removed container alpha-dictionaryetl-0a1b2c3d, which an earlier run left"
	}) {
		t.Errorf("no warning naming the removed container in %v", rec.Events())
	}
	if comp.downs != 1 {
		t.Errorf("compose down ran %d times", comp.downs)
	}
}

func withLabel(labels map[string]string, k, v string) map[string]string {
	l := maps.Clone(labels)
	l[k] = v
	return l
}
