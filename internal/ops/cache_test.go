package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

var cacheNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

const (
	shaA = "aaaaaaaaaaaa1111111111111111111111111111"
	shaB = "bbbbbbbbbbbb2222222222222222222222222222"
	shaC = "cccccccccccc3333333333333333333333333333"
	shaD = "dddddddddddd4444444444444444444444444444"
)

// fakeDaemon answers the docker commands cache list and prune run.
type fakeDaemon struct {
	images     []fakeImage
	containers []fakeContainer
	volumes    map[string]map[string]string // name: labels
	networks   map[string]map[string]string // name: labels
	rmFail     map[string]string            // ref: docker's error
}

type fakeImage struct {
	ref, id string
	size    int64
	created time.Time
}

type fakeContainer struct {
	name, image, imageID string
	labels               map[string]string
	binds                []string
}

func (fd *fakeDaemon) runner(t *testing.T) *fakerunner.Runner {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		return fd.answer(c.Argv)
	})
	return f
}

func (fd *fakeDaemon) answer(argv []string) (docker.Result, error) {
	out := func(v any) (docker.Result, error) {
		b, err := json.Marshal(v)
		return docker.Result{Stdout: append(b, '\n')}, err
	}
	switch strings.Join(argv[1:3], " ") {
	case "ps -a":
		var ids []string
		for _, c := range fd.containers {
			ids = append(ids, "id-"+c.name)
		}
		return docker.Result{Stdout: []byte(strings.Join(ids, "\n"))}, nil
	case "rm -v":
		if msg, ok := fd.rmFail[argv[len(argv)-1]]; ok {
			return docker.Result{ExitCode: 1, Stderr: []byte("Error response from daemon: " + msg)}, nil
		}
		fd.containers = slices.DeleteFunc(fd.containers, func(c fakeContainer) bool { return c.name == argv[len(argv)-1] })
		return docker.Result{}, nil
	case "container inspect":
		var res []any
		for _, id := range argv[3:] {
			for _, c := range fd.containers {
				if "id-"+c.name != id {
					continue
				}
				var mounts []map[string]string
				for _, b := range c.binds {
					mounts = append(mounts, map[string]string{"Type": "bind", "Source": b})
				}
				res = append(res, map[string]any{"Id": id, "Name": "/" + c.name, "Image": c.imageID,
					"Config": map[string]any{"Image": c.image, "Labels": c.labels},
					"State":  map[string]any{"Status": "exited"}, "Mounts": mounts})
			}
		}
		return out(res)
	case "volume ls":
		var names []string
		for n := range fd.volumes {
			names = append(names, n)
		}
		return docker.Result{Stdout: []byte(strings.Join(names, "\n"))}, nil
	case "volume inspect":
		var res []any
		for _, n := range argv[3:] {
			res = append(res, map[string]any{"Name": n, "Labels": fd.volumes[n]})
		}
		return out(res)
	case "network ls":
		var names []string
		for n := range fd.networks {
			names = append(names, n)
		}
		return docker.Result{Stdout: []byte(strings.Join(names, "\n"))}, nil
	case "network inspect":
		var res []any
		for _, n := range argv[3:] {
			res = append(res, map[string]any{"Name": n, "Labels": fd.networks[n]})
		}
		return out(res)
	case "image ls":
		var refs []string
		for _, i := range fd.images {
			refs = append(refs, i.ref)
		}
		return docker.Result{Stdout: []byte(strings.Join(refs, "\n"))}, nil
	case "image inspect":
		var res []any
		for _, ref := range argv[3:] {
			i := fd.image(ref)
			var tags []string
			for _, j := range fd.images {
				if j.id == i.id {
					tags = append(tags, j.ref)
				}
			}
			res = append(res, map[string]any{"Id": i.id, "RepoTags": tags, "Size": i.size, "Created": i.created})
		}
		return out(res)
	case "image rm":
		if msg, ok := fd.rmFail[argv[3]]; ok {
			return docker.Result{ExitCode: 1, Stderr: []byte("Error response from daemon: " + msg)}, nil
		}
		fd.images = slices.DeleteFunc(fd.images, func(i fakeImage) bool { return i.ref == argv[3] })
		return docker.Result{}, nil
	}
	return docker.Result{ExitCode: 1, Stderr: []byte("unexpected " + strings.Join(argv, " "))}, nil
}

func (fd *fakeDaemon) image(ref string) fakeImage {
	for _, i := range fd.images {
		if i.ref == ref {
			return i
		}
	}
	return fakeImage{}
}

func (fd *fakeDaemon) refs() []string {
	var refs []string
	for _, i := range fd.images {
		refs = append(refs, i.ref)
	}
	return refs
}

// cacheFixture is a cache, a daemon and two stacks: "alpha", readable, and
// "gone", labelled on a stopped container but moved away.
type cacheFixture struct {
	cache  *cache.Cache
	daemon *fakeDaemon
	alpha  string // alpha's stack directory
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	base := t.TempDir()
	c, err := cache.Open(filepath.Join(base, "cache"), cache.Options{LockTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	old := cacheNow.Add(-48 * time.Hour)
	mk := func(rel string, mtime time.Time) {
		p := filepath.Join(c.Root(), filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "f"), []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	mk("src/pic-sure/"+shaA, old) // alpha's state names it
	mk("src/pic-sure/"+shaB, old) // a container mounts a path inside it
	mk("src/pic-sure/"+shaC, old) // nothing known uses it
	mk("src/PIC-SURE-Frontend/"+shaD, old)
	mk("build/cccccccccccc", old)
	mk("downloads/nhanes", old)
	mk("tmp/load-old", old)
	mk("tmp/load-new", cacheNow.Add(-time.Minute))

	alpha, err := stack.Create(filepath.Join(base, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = alpha.Close() }()
	if err := alpha.WriteFile(stack.ConfigFile, []byte("schema: 1\nname: alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := alpha.SaveState(&stack.State{
		Components: map[string]stack.Component{"pic-sure": {Commit: shaA}},
		Images:     map[string]string{"pic-sure-hpds": "aaaaaaaaaaaa"},
		DevImages:  map[string]string{"pic-sure-psama": "dev-alpha-aaaaaaaaaaaa"},
	}); err != nil {
		t.Fatal(err)
	}

	gone := filepath.Join(base, "moved-away")
	fd := &fakeDaemon{
		images: []fakeImage{
			{"hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa", "sha256:hpds-a", 400, old},
			{"hms-dbmi/pic-sure-gateway:bbbbbbbbbbbb", "sha256:gw-b", 300, old},
			{"hms-dbmi/pic-sure-gateway:other-tool", "sha256:gw-b", 300, old},
			{"hms-dbmi/pic-sure-psama:cccccccccccc", "sha256:psama-c", 200, old},
			{"hms-dbmi/pic-sure-psama:dev-alpha-aaaaaaaaaaaa", "sha256:psama-dev", 200, old},
			{"hms-dbmi/pic-sure-psama:dev-gone-aaaaaaaaaaaa", "sha256:psama-gone", 200, old},
			{"hms-dbmi/pic-sure-psama:dev-zed-aaaaaaaaaaaa-dirty", "sha256:psama-zed", 100, old},
			{"hms-dbmi/pic-sure-httpd:dev-new-dddddddddddd", "sha256:httpd-new", 500, cacheNow.Add(-10 * time.Minute)},
			// Just built by a stack prune can't see; "gone" might use it too.
			{"hms-dbmi/pic-sure-visualization:eeeeeeeeeeee", "sha256:vis-new", 500, cacheNow.Add(-10 * time.Minute)},
		},
		containers: []fakeContainer{
			// Another tool's stack, unlabelled, running the gateway by tag;
			// docker reports its image by ID.
			{name: "other-gateway-1", image: "hms-dbmi/pic-sure-gateway:other-tool", imageID: "sha256:gw-b",
				binds: []string{filepath.Join(c.Root(), "src/pic-sure", shaB, "conf")}},
			{name: "gone-hpds-1", image: "alpine:3", imageID: "sha256:alpine",
				labels: map[string]string{stack.LabelStack: "gone", stack.LabelStackDir: gone}},
		},
		volumes: map[string]map[string]string{
			"alpha_hpds-data": {stack.LabelStack: "alpha", stack.LabelStackDir: alpha.Dir},
		},
	}
	return &cacheFixture{cache: c, daemon: fd, alpha: alpha.Dir}
}

func (fx *cacheFixture) deps(t *testing.T, rec *events.Recorder) (*ops.Deps, *fakerunner.Runner) {
	f := fx.daemon.runner(t)
	sink := events.Sink(events.Discard)
	if rec != nil {
		sink = rec
	}
	return &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Clock: ops.FixedClock(cacheNow), Sink: sink}, f
}

func statuses(items []ops.CacheItem) map[string]string {
	m := map[string]string{}
	for _, it := range items {
		m[it.Name] = it.Status
	}
	return m
}

func TestCacheInventoryInUseRules(t *testing.T) {
	fx := newCacheFixture(t)
	d, _ := fx.deps(t, nil)
	r, err := ops.CacheInventory(context.Background(), d, fx.cache, ops.CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa":                ops.CacheInUse,        // alpha's state
		"hms-dbmi/pic-sure-gateway:bbbbbbbbbbbb":             ops.CacheInUse,        // a container runs its image ID
		"hms-dbmi/pic-sure-psama:cccccccccccc":               ops.CacheUnknownStack, // "gone" might use it
		"hms-dbmi/pic-sure-psama:dev-alpha-aaaaaaaaaaaa":     ops.CacheInUse,        // alpha's dev_images
		"hms-dbmi/pic-sure-psama:dev-gone-aaaaaaaaaaaa":      ops.CacheUnknownStack, // gone's own dev image
		"hms-dbmi/pic-sure-psama:dev-zed-aaaaaaaaaaaa-dirty": ops.CacheUnused,       // no stack zed
		"hms-dbmi/pic-sure-httpd:dev-new-dddddddddddd":       ops.CacheRecent,
		"hms-dbmi/pic-sure-visualization:eeeeeeeeeeee":       ops.CacheRecent, // recent wins over unknown_stack
		"src/pic-sure/" + shaA:                               ops.CacheInUse,
		"src/pic-sure/" + shaB:                               ops.CacheInUse,
		"src/pic-sure/" + shaC:                               ops.CacheUnknownStack,
		"src/PIC-SURE-Frontend/" + shaD:                      ops.CacheUnknownStack,
		"build/cccccccccccc":                                 ops.CacheUnused,
		"downloads/nhanes":                                   ops.CacheUnused,
		"tmp/load-old":                                       ops.CacheUnused,
		"tmp/load-new":                                       ops.CacheRecent,
	}
	got := statuses(r.Items)
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: status %q, want %q", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("items %v, want exactly %d (the other-tool tag isn't managed)", got, len(want))
	}

	if len(r.Stacks) != 2 {
		t.Fatalf("stacks %+v, want alpha and gone", r.Stacks)
	}
	for _, s := range r.Stacks {
		switch s.Name {
		case "alpha":
			if !s.Readable {
				t.Errorf("alpha unreadable: %s", s.Error)
			}
		case "gone":
			if s.Readable || s.Error == "" {
				t.Errorf("gone: %+v, want unreadable with a reason", s)
			}
		default:
			t.Errorf("unexpected stack %+v", s)
		}
	}
	for _, it := range r.Items {
		if it.Name == "src/pic-sure/"+shaB && !slices.Contains(it.UsedBy, "container other-gateway-1") {
			t.Errorf("shaB used by %v, want the container that mounts it", it.UsedBy)
		}
	}
}

// A stack found only through CacheOptions (built, never started) protects
// what its state names.
func TestCacheInventoryCountsTheCurrentStack(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.volumes = nil
	d, _ := fx.deps(t, nil)
	r, err := ops.CacheInventory(context.Background(), d, fx.cache, ops.CacheOptions{Stacks: []string{fx.alpha}})
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(r.Items)["hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa"]; s != ops.CacheInUse {
		t.Errorf("alpha's image is %s, want in_use", s)
	}
}

func TestPruneCacheDryRunRemovesNothing(t *testing.T) {
	fx := newCacheFixture(t)
	d, f := fx.deps(t, nil)
	before := fx.daemon.refs()
	r, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker image rm *"))
	if !slices.Equal(fx.daemon.refs(), before) {
		t.Errorf("images changed: %v", fx.daemon.refs())
	}
	var names []string
	for _, it := range r.Removed {
		names = append(names, it.Name)
		if _, err := os.Stat(filepath.Join(fx.cache.Root(), it.Name)); it.Kind != ops.CacheImage && err != nil {
			t.Errorf("dry run removed %s", it.Name)
		}
	}
	want := []string{"hms-dbmi/pic-sure-psama:dev-zed-aaaaaaaaaaaa-dirty", "build/cccccccccccc", "downloads/nhanes", "tmp/load-old"}
	if !slices.Equal(names, want) {
		t.Errorf("would remove %v, want %v", names, want)
	}
	if r.Freed != 100+3*5 {
		t.Errorf("freed %d, want %d", r.Freed, 100+3*5)
	}
}

func TestPruneCacheRemovesOnlyUnused(t *testing.T) {
	fx := newCacheFixture(t)
	var rec events.Recorder
	d, f := fx.deps(t, &rec)
	if _, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if rm := f.CallsMatching(fakerunner.Glob("docker image rm *")); len(rm) != 1 || rm[0].Argv[3] != "hms-dbmi/pic-sure-psama:dev-zed-aaaaaaaaaaaa-dirty" {
		t.Errorf("image rm calls %v, want only the zed dev image", rm)
	}
	for rel, kept := range map[string]bool{
		"src/pic-sure/" + shaA: true, "src/pic-sure/" + shaB: true, "src/pic-sure/" + shaC: true,
		"src/PIC-SURE-Frontend/" + shaD: true, "tmp/load-new": true,
		"build/cccccccccccc": false, "downloads/nhanes": false, "tmp/load-old": false,
	} {
		_, err := os.Stat(filepath.Join(fx.cache.Root(), rel))
		if kept != (err == nil) {
			t.Errorf("%s: kept=%v, want %v", rel, err == nil, kept)
		}
	}
	for _, dir := range []string{"locks", "git"} {
		if _, err := os.Stat(filepath.Join(fx.cache.Root(), dir)); err != nil {
			t.Errorf("%s removed: %v", dir, err)
		}
	}
	var warned bool
	for _, e := range rec.Events() {
		if w, ok := e.(events.Warning); ok && strings.Contains(w.Text, "moved-away") && strings.Contains(w.Text, "--force") {
			warned = true
		}
	}
	if !warned {
		t.Error("no warning naming the unreadable stack and --force")
	}
}

func TestPruneCacheForceStillKeepsInUse(t *testing.T) {
	fx := newCacheFixture(t)
	d, _ := fx.deps(t, nil)
	if _, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa",
		"hms-dbmi/pic-sure-gateway:bbbbbbbbbbbb",
		"hms-dbmi/pic-sure-gateway:other-tool", // not managed
		"hms-dbmi/pic-sure-psama:dev-alpha-aaaaaaaaaaaa",
		"hms-dbmi/pic-sure-httpd:dev-new-dddddddddddd", // recent
		"hms-dbmi/pic-sure-visualization:eeeeeeeeeeee", // recent, though gone might use it
	}
	if got := fx.daemon.refs(); !slices.Equal(got, want) {
		t.Errorf("images left %v, want %v", got, want)
	}
	for rel, kept := range map[string]bool{
		"src/pic-sure/" + shaA: true, "src/pic-sure/" + shaB: true, "tmp/load-new": true,
		"src/pic-sure/" + shaC: false, "src/PIC-SURE-Frontend/" + shaD: false,
	} {
		_, err := os.Stat(filepath.Join(fx.cache.Root(), rel))
		if kept != (err == nil) {
			t.Errorf("%s: kept=%v, want %v", rel, err == nil, kept)
		}
	}
}

// An image whose other tags all go counts once in Freed; one that keeps a
// tag counts nothing. A failed removal doesn't stop the rest.
func TestPruneCacheFreedAndFailures(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.containers = nil
	fx.daemon.volumes = nil
	fx.daemon.images = []fakeImage{
		{"hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa", "sha256:x", 1000, time.Time{}},
		{"hms-dbmi/pic-sure-hpds:dev-s-aaaaaaaaaaaa", "sha256:x", 1000, time.Time{}},
		{"hms-dbmi/pic-sure-gateway:bbbbbbbbbbbb", "sha256:y", 700, time.Time{}},
		{"hms-dbmi/pic-sure-gateway:latest", "sha256:y", 700, time.Time{}},
		{"hms-dbmi/pic-sure-psama:cccccccccccc", "sha256:z", 50, time.Time{}},
	}
	fx.daemon.rmFail = map[string]string{"hms-dbmi/pic-sure-psama:cccccccccccc": "conflict: image is being used"}
	for _, rel := range []string{"src/pic-sure/" + shaA, "src/pic-sure/" + shaB, "src/pic-sure/" + shaC, "src/PIC-SURE-Frontend/" + shaD, "tmp/load-new"} {
		if err := os.RemoveAll(filepath.Join(fx.cache.Root(), rel)); err != nil {
			t.Fatal(err)
		}
	}
	d, _ := fx.deps(t, nil)
	r, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{})
	if err == nil || !strings.Contains(err.Error(), "pic-sure-psama:cccccccccccc") {
		t.Fatalf("err = %v, want one naming the image docker refused", err)
	}
	if len(r.Skipped) != 1 || len(r.Removed) != 6 {
		t.Errorf("removed %d, skipped %+v; want 6 and the psama image", len(r.Removed), r.Skipped)
	}
	if want := int64(1000 + 3*5); r.Freed != want {
		t.Errorf("freed %d, want %d", r.Freed, want)
	}
}

// A stack whose only labelled resource left is a network still counts.
func TestCacheInventoryFindsAStackByItsNetwork(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.volumes = nil
	fx.daemon.networks = map[string]map[string]string{
		"alpha_default": {stack.LabelStack: "alpha", stack.LabelStackDir: fx.alpha},
	}
	d, _ := fx.deps(t, nil)
	r, err := ops.CacheInventory(context.Background(), d, fx.cache, ops.CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(r.Items)["hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa"]; s != ops.CacheInUse {
		t.Errorf("alpha's image is %s, want in_use", s)
	}
}

// A stack that hasn't saved state.json yet names nothing, and doesn't
// block prune like an unreadable one.
func TestCacheInventoryStackWithoutStateNamesNothing(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.containers = nil
	fresh, err := stack.Create(filepath.Join(t.TempDir(), "fresh"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if err := fresh.WriteFile(stack.ConfigFile, []byte("schema: 1\nname: fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, _ := fx.deps(t, nil)
	r, err := ops.CacheInventory(context.Background(), d, fx.cache, ops.CacheOptions{Stacks: []string{fresh.Dir}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.Stacks {
		if !s.Readable {
			t.Errorf("stack %s unreadable: %s", s.Dir, s.Error)
		}
	}
	if s := statuses(r.Items)["hms-dbmi/pic-sure-psama:cccccccccccc"]; s != ops.CacheUnused {
		t.Errorf("an image no stack names is %s, want unused", s)
	}
}

// While an image step holds the use lock, prune removes nothing; a dry run
// still reports.
func TestPruneCacheWaitsForImageSteps(t *testing.T) {
	fx := newCacheFixture(t)
	builder, err := cache.Open(fx.cache.Root(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	use, err := builder.LockUse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = use.Unlock() }()

	d, f := fx.deps(t, nil)
	if _, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{}); !errors.Is(err, cache.ErrLockTimeout) ||
		!strings.Contains(err.Error(), "a build is using the cache") {
		t.Fatalf("err = %v, want a lock timeout saying a build is running", err)
	}
	f.AssertNotCalled(fakerunner.Glob("docker image rm *"))
	if _, err := os.Stat(filepath.Join(fx.cache.Root(), "downloads/nhanes")); err != nil {
		t.Errorf("download removed: %v", err)
	}
	if r, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{DryRun: true}); err != nil || len(r.Removed) == 0 {
		t.Errorf("dry run: %d items, %v", len(r.Removed), err)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 999: "999 B", 1000: "1.00 kB", 1234: "1.23 kB", 99_949: "99.9 kB", 999_499: "999 kB",
		999_999: "1.00 MB", 424_693_812: "425 MB", 1_150_000_000: "1.15 GB",
	} {
		if got := ops.FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// A registered stack with no labelled resource keeps what its state names.
func TestCacheInventoryCountsARegisteredStack(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.volumes = nil
	if err := fx.cache.RegisterStack(context.Background(), fx.alpha, "alpha"); err != nil {
		t.Fatal(err)
	}
	d, _ := fx.deps(t, nil)
	r, err := ops.CacheInventory(context.Background(), d, fx.cache, ops.CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := statuses(r.Items)
	for _, name := range []string{"hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa", "hms-dbmi/pic-sure-psama:dev-alpha-aaaaaaaaaaaa", "src/pic-sure/" + shaA} {
		if st[name] != ops.CacheInUse {
			t.Errorf("%s is %s, want in_use", name, st[name])
		}
	}
	i := slices.IndexFunc(r.Stacks, func(s ops.CacheStack) bool { return s.Dir == fx.alpha })
	if i < 0 || !r.Stacks[i].Registered || !r.Stacks[i].Readable || r.Stacks[i].Name != "alpha" {
		t.Errorf("stacks %+v, want alpha registered and readable", r.Stacks)
	}
}

// A registered stack whose directory is gone and that has no labelled
// resource protects nothing, and prune forgets it; a dry run only says so.
func TestPruneCacheForgetsAGoneStack(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.containers = nil // "gone"'s labelled container
	gone := filepath.Join(t.TempDir(), "deleted")
	if err := fx.cache.RegisterStack(context.Background(), gone, "deleted"); err != nil {
		t.Fatal(err)
	}
	d, _ := fx.deps(t, nil)

	r, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Forgotten) != 1 || r.Forgotten[0].Dir != gone || !r.Forgotten[0].Gone {
		t.Fatalf("dry run would forget %+v, want %s", r.Forgotten, gone)
	}
	if st := statuses(r.Items)["hms-dbmi/pic-sure-psama:cccccccccccc"]; st != ops.CacheUnused {
		t.Errorf("an image only the gone stack might use is %s, want unused", st)
	}
	if reg, _ := fx.cache.RegisteredStacks(); len(reg) != 1 {
		t.Fatalf("dry run changed the registry: %+v", reg)
	}

	if _, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if reg, _ := fx.cache.RegisteredStacks(); len(reg) != 0 {
		t.Errorf("registry after prune: %+v, want empty", reg)
	}
}

// A registered stack whose directory is gone but whose labelled resources
// remain has moved: it blocks what it might use, and stays registered.
func TestPruneCacheKeepsAMovedRegisteredStack(t *testing.T) {
	fx := newCacheFixture(t)
	gone := fx.daemon.containers[1].labels[stack.LabelStackDir]
	if err := fx.cache.RegisterStack(context.Background(), gone, "gone"); err != nil {
		t.Fatal(err)
	}
	d, _ := fx.deps(t, nil)
	r, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Forgotten) != 0 {
		t.Errorf("forgot %+v, want nothing", r.Forgotten)
	}
	if st := statuses(r.Items)["hms-dbmi/pic-sure-psama:cccccccccccc"]; st != ops.CacheUnknownStack {
		t.Errorf("an image the moved stack might use is %s, want unknown_stack", st)
	}
	if reg, _ := fx.cache.RegisteredStacks(); len(reg) != 1 {
		t.Errorf("registry after prune: %+v, want the moved stack", reg)
	}
}

// A registry entry that can't be read blocks what any stack might use, like
// an unreadable stack, and only --force forgets it.
func TestPruneCacheKeepsAnUnreadableRegistryEntry(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.containers = nil
	entry := filepath.Join(fx.cache.Root(), "stacks", "broken")
	if err := os.WriteFile(entry, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d, _ := fx.deps(t, nil)
	r, err := ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st := statuses(r.Items)["hms-dbmi/pic-sure-psama:cccccccccccc"]; st != ops.CacheUnknownStack {
		t.Errorf("an image is %s, want unknown_stack", st)
	}
	if len(r.Forgotten) != 0 {
		t.Errorf("forgot %+v without --force", r.Forgotten)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("entry removed without --force: %v", err)
	}
	if r, err = ops.PruneCache(context.Background(), d, fx.cache, ops.PruneOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(r.Forgotten) != 1 || ops.ForgottenStack(r.Forgotten[0]) != "the unreadable registry entry stacks/broken" {
		t.Errorf("forgot %+v, want the broken entry", r.Forgotten)
	}
	if _, err := os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("entry still there with --force: %v", err)
	}
}

// A registered directory that lost pic-sure.yaml but still has .pic-sure/
// isn't gone: its state may name what it uses.
func TestCacheInventoryKeepsARegisteredStackWithoutItsConfig(t *testing.T) {
	fx := newCacheFixture(t)
	fx.daemon.volumes, fx.daemon.containers = nil, nil
	if err := fx.cache.RegisterStack(context.Background(), fx.alpha, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fx.alpha, stack.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	d, _ := fx.deps(t, nil)
	r, err := ops.CacheInventory(context.Background(), d, fx.cache, ops.CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Stacks) != 1 || r.Stacks[0].Gone || r.Stacks[0].Readable {
		t.Fatalf("stacks %+v, want alpha unreadable, not gone", r.Stacks)
	}
	if st := statuses(r.Items)["hms-dbmi/pic-sure-hpds:aaaaaaaaaaaa"]; st != ops.CacheUnknownStack {
		t.Errorf("alpha's image is %s, want unknown_stack", st)
	}
}
