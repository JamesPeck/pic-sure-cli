package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

type reactorFixture struct {
	f    *fakerunner.Runner
	rec  *events.Recorder
	d    *Deps
	opts ReactorOptions
	root string
}

func newReactorFixture(t *testing.T) *reactorFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cache")
	c, err := cache.Open(root, cache.Options{Holder: "test"})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for _, img := range catalog.ImagesBuiltFrom(catalog.PicSure) {
		if err := os.MkdirAll(filepath.Join(src, img.Context), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := fakerunner.New(t)
	rec := &events.Recorder{}
	return &reactorFixture{
		f:    f,
		rec:  rec,
		d:    &Deps{Runner: f, Docker: docker.NewEngine(f), Sink: rec, Rand: strings.NewReader(strings.Repeat("r", 64))},
		opts: ReactorOptions{Cache: c, SHA: testSHA, Source: src, Step: "build"},
		root: root,
	}
}

func imageJSON(labels map[string]string) string {
	var pairs []string
	for k, v := range labels {
		pairs = append(pairs, fmt.Sprintf("%q:%q", k, v))
	}
	return `[{"Id":"sha256:abc","Config":{"Labels":{` + strings.Join(pairs, ",") + `}}}]`
}

// fresh makes the named reactor images exist, built from testSHA.
func (x *reactorFixture) fresh(names ...string) {
	for _, n := range names {
		x.f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/"+n+":0123456789ab")).
			Stdout(imageJSON(map[string]string{ReactorSrcLabel: testSHA}))
	}
}

// missing makes every other reactor image absent.
func (x *reactorFixture) missing() {
	x.f.On(fakerunner.Glob("docker image inspect hms-dbmi/*")).Exit(1).Stderr("Error: No such image: x")
}

// reactorOK makes the Maven run succeed with no reactor container in the way.
func (x *reactorFixture) reactorOK() {
	x.f.On(fakerunner.Glob("docker image inspect maven:*")).Stdout(imageJSON(nil))
	x.f.On(fakerunner.Exact("docker", "volume", "create", cache.MavenVolume))
	x.f.On(fakerunner.Exact("docker", "container", "inspect", ReactorContainer)).
		Exit(1).Stderr("Error: No such container: " + ReactorContainer)
	x.f.On(fakerunner.Glob("docker run -d *")).Stdout("ctr123\n")
	x.f.On(fakerunner.Glob("docker exec ctr123 sh -c *"))
	x.f.On(fakerunner.Glob("docker cp ctr123:*"))
	x.f.On(fakerunner.Exact("docker", "rm", "-v", "-f", "ctr123"))
	x.f.On(fakerunner.Glob("docker build *"))
}

func allImageNames() []string {
	var names []string
	for _, img := range catalog.ImagesBuiltFrom(catalog.PicSure) {
		names = append(names, img.Name)
	}
	return names
}

func TestReactorBuildIsANoOpWhenEveryImageIsUpToDate(t *testing.T) {
	x := newReactorFixture(t)
	x.fresh(allImageNames()...)

	res, err := BuildReactor(context.Background(), x.d, x.opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tag != "0123456789ab" || len(res.Built) != 0 {
		t.Errorf("result = %+v, want tag 0123456789ab and nothing built", res)
	}
	if n := len(x.f.Calls()); n != 11 {
		t.Errorf("made %d calls, want only the 11 image inspects:\n%v", n, x.f.Calls())
	}
	up, err := ReactorUpToDate(context.Background(), x.d, testSHA, "0123456789ab")
	if err != nil || !up {
		t.Errorf("ReactorUpToDate = %v, %v; want true", up, err)
	}
}

func TestReactorBuildRunsMavenThenBuildsEveryImage(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	x.reactorOK()

	res, err := BuildReactor(context.Background(), x.d, x.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(res.Built, " "), strings.Join(allImageNames(), " "); got != want {
		t.Errorf("built %s\nwant  %s", got, want)
	}

	run := x.f.CallsMatching(fakerunner.Glob("docker run -d *"))[0].String()
	for _, want := range []string{
		"--name " + ReactorContainer,
		"-v " + x.opts.Source + ":/src:ro",
		"-v pic-sure-m2:/root/.m2",
		"maven:3-amazoncorretto-25 sh -c",
		reactorRunLabel + "=run-",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("run %s\nlacks %q", run, want)
		}
	}
	x.f.AssertCalled(fakerunner.Exact("docker", "exec", "ctr123",
		"sh", "-c", "cp -r /src/. /build && exec mvn -B install -T1C -DskipTests"))

	buildDir := filepath.Join(x.root, "build", "0123456789ab")
	order := []fakerunner.Matcher{
		fakerunner.Exact("docker", "volume", "create", cache.MavenVolume),
		fakerunner.Glob("docker run -d *"),
		fakerunner.Glob("docker exec ctr123 sh -c *"),
	}
	// Overlapping contexts are copied once.
	for _, c := range []string{
		"services/pic-sure-auth-microapp",
		"services/pic-sure-gateway",
		"services/pic-sure-hpds",
		"services/pic-sure-hpds-query-service",
		"services/pic-sure-logging",
		"services/pic-sure-operations-service",
		"services/pic-sure-visualization-service",
		"services/picsure-dictionary",
	} {
		order = append(order, fakerunner.Exact("docker", "cp", "ctr123:/build/"+c+"/.", filepath.Join(buildDir, c)))
	}
	if n := len(x.f.CallsMatching(fakerunner.Glob("docker cp *"))); n != 8 {
		t.Errorf("made %d copies, want 8", n)
	}
	order = append(order, fakerunner.Exact("docker", "rm", "-v", "-f", "ctr123"))
	for _, img := range catalog.ImagesBuiltFrom(catalog.PicSure) {
		order = append(order, fakerunner.Exact("docker", "build",
			"-f", filepath.Join(buildDir, img.Dockerfile),
			"-t", "hms-dbmi/"+img.Name+":0123456789ab",
			"--label", ReactorSrcLabel+"="+testSHA,
			filepath.Join(buildDir, img.Context)))
	}
	x.f.AssertOrder(order...)

	if _, err := os.Stat(buildDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("build dir left behind: %v", err)
	}
}

func TestReactorBuildBuildsOnlyStaleImages(t *testing.T) {
	x := newReactorFixture(t)
	var fresh []string
	for _, n := range allImageNames() {
		if n != "pic-sure-gateway" && n != "pic-sure-dictionary-dump" {
			fresh = append(fresh, n)
		}
	}
	x.fresh(fresh...)
	// The dump image exists but was built from another commit.
	x.f.On(fakerunner.Exact("docker", "image", "inspect", "hms-dbmi/pic-sure-dictionary-dump:0123456789ab")).
		Stdout(imageJSON(map[string]string{ReactorSrcLabel: "other"}))
	x.missing()
	x.reactorOK()

	res, err := BuildReactor(context.Background(), x.d, x.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Built, " "); got != "pic-sure-gateway pic-sure-dictionary-dump" {
		t.Errorf("built %s", got)
	}
	if n := len(x.f.CallsMatching(fakerunner.Glob("docker cp *"))); n != 2 {
		t.Errorf("made %d copies, want 2", n)
	}
	if n := len(x.f.CallsMatching(fakerunner.Glob("docker build *"))); n != 2 {
		t.Errorf("ran %d builds, want 2", n)
	}
}

func TestReactorBuildForceRebuildsFreshImages(t *testing.T) {
	x := newReactorFixture(t)
	x.fresh(allImageNames()...)
	x.reactorOK()
	x.opts.Force = true

	res, err := BuildReactor(context.Background(), x.d, x.opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Built) != 11 {
		t.Errorf("built %v, want all 11", res.Built)
	}
}

func TestReactorBuildFailureShowsTheTailAndRemovesTheContainer(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Glob("docker exec ctr123 sh -c *")).
		Stdout("[INFO] Building pic-sure-gateway 1.0-SNAPSHOT    [3/40]\n[ERROR] compilation failed\n").Exit(1)
	x.reactorOK()
	x.opts.LogDir = t.TempDir()

	_, err := BuildReactor(context.Background(), x.d, x.opts)
	if err == nil || !strings.Contains(err.Error(), "exit code 1") ||
		!strings.Contains(err.Error(), filepath.Join(x.opts.LogDir, "pic-sure-reactor.log")) {
		t.Fatalf("err = %v, want the exit code and the log path", err)
	}
	x.f.AssertCalled(fakerunner.Exact("docker", "rm", "-v", "-f", "ctr123"))
	x.f.AssertNotCalled(fakerunner.Glob("docker cp *"))
	x.f.AssertNotCalled(fakerunner.Glob("docker build *"))

	var progress, tail bool
	for _, e := range x.rec.Events() {
		switch e := e.(type) {
		case events.Progress:
			progress = progress || e.Text == "Maven: building pic-sure-gateway (3/40)"
		case events.Log:
			tail = tail || e.Line == "[ERROR] compilation failed"
		}
	}
	if !progress || !tail {
		t.Errorf("module progress %v, error tail %v; want both", progress, tail)
	}
	log, _ := os.ReadFile(filepath.Join(x.opts.LogDir, "pic-sure-reactor.log"))
	if !strings.Contains(string(log), "compilation failed") {
		t.Errorf("log = %q", log)
	}
}

func TestReactorBuildRemovesAStoppedReactorContainer(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Exact("docker", "container", "inspect", ReactorContainer)).Times(1).
		Stdout(`[{"Id":"old","State":{"Status":"exited","Running":false}}]`)
	x.f.On(fakerunner.Exact("docker", "rm", "-v", "-f", "old"))
	x.reactorOK()

	if _, err := BuildReactor(context.Background(), x.d, x.opts); err != nil {
		t.Fatal(err)
	}
	x.f.AssertOrder(fakerunner.Exact("docker", "rm", "-v", "-f", "old"), fakerunner.Glob("docker run -d *"))
}

func TestReactorBuildWaitsForARunningReactorContainer(t *testing.T) {
	defer func(p time.Duration) { reactorPoll = p }(reactorPoll)
	reactorPoll = time.Millisecond
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Exact("docker", "container", "inspect", ReactorContainer)).Times(2).
		Stdout(`[{"Id":"theirs","State":{"Status":"running","Running":true}}]`)
	x.reactorOK()

	if _, err := BuildReactor(context.Background(), x.d, x.opts); err != nil {
		t.Fatal(err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker rm * theirs"))
	waits := 0
	for _, e := range x.rec.Events() {
		if p, ok := e.(events.Progress); ok && strings.HasPrefix(p.Text, "Waiting for another reactor build") {
			waits++
		}
	}
	if waits != 1 {
		t.Errorf("got %d waiting events, want 1", waits)
	}
}

func TestReactorBuildStopsWaitingWhenCancelled(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Glob("docker image inspect maven:*")).Stdout(imageJSON(nil))
	x.f.On(fakerunner.Exact("docker", "volume", "create", cache.MavenVolume))
	ctx, cancel := context.WithCancel(context.Background())
	x.f.On(fakerunner.Exact("docker", "container", "inspect", ReactorContainer)).
		Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
			cancel()
			return docker.Result{Stdout: []byte(`[{"Id":"theirs","State":{"Running":true}}]`)}, nil
		})

	_, err := BuildReactor(ctx, x.d, x.opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run -d *"))
}

func TestReactorBuildPassesTheProxy(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	x.reactorOK()
	p, err := netproxy.New(netproxy.Config{HTTPS: "http://proxy.example:3128"}, netproxy.CatalogServices())
	if err != nil {
		t.Fatal(err)
	}
	x.opts.Proxy = p

	if _, err := BuildReactor(context.Background(), x.d, x.opts); err != nil {
		t.Fatal(err)
	}
	run := x.f.CallsMatching(fakerunner.Glob("docker run -d *"))[0].String()
	exec := x.f.CallsMatching(fakerunner.Glob("docker exec *"))[0].String()
	if !strings.Contains(run, ":/pic-sure/settings.xml:ro") || !strings.Contains(exec, "-s /pic-sure/settings.xml") {
		t.Errorf("run %s\nexec %s\nlack the Maven settings", run, exec)
	}
	build := x.f.CallsMatching(fakerunner.Glob("docker build *"))[0]
	if !strings.Contains(build.String(), "--build-arg HTTPS_PROXY") || !build.HasEnv("HTTPS_PROXY") {
		t.Errorf("build %s (env %v) lacks the proxy build arg", build, build.Env)
	}
	// The settings file holds credentials; it is gone once Maven has run.
	if left, _ := filepath.Glob(filepath.Join(x.root, "tmp", "maven-settings-*")); len(left) != 0 {
		t.Errorf("settings left behind: %v", left)
	}
}

func TestReactorBuildExplainsHPDSETLAlpinePins(t *testing.T) {
	x := newReactorFixture(t)
	var fresh []string
	for _, n := range allImageNames() {
		if n != "pic-sure-hpds-etl" {
			fresh = append(fresh, n)
		}
	}
	x.fresh(fresh...)
	x.missing()
	x.f.On(fakerunner.Glob("docker build * -t hms-dbmi/pic-sure-hpds-etl:*")).
		Stderr("ERROR: unable to select packages:\n  bash-5.2.0-r0:\n    breaks: world[bash=5.2.0-r0]\n").Exit(1)
	x.reactorOK()

	_, err := BuildReactor(context.Background(), x.d, x.opts)
	if err == nil || !strings.Contains(err.Error(), "Alpine packages the index no longer has") {
		t.Fatalf("err = %v, want the Alpine hint", err)
	}
}

func TestReactorBuildRechecksTheImagesOnceItHasTheLock(t *testing.T) {
	x := newReactorFixture(t)
	// Missing on the first look, then built by the command that held the
	// lock.
	x.f.On(fakerunner.Glob("docker image inspect hms-dbmi/*")).Times(11).Exit(1).Stderr("Error: No such image: x")
	x.fresh(allImageNames()...)

	res, err := BuildReactor(context.Background(), x.d, x.opts)
	if err != nil || len(res.Built) != 0 {
		t.Fatalf("BuildReactor = %+v, %v; want nothing built", res, err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run *"))
}

func TestReactorBuildRefusesAPreMonorepoCommit(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	if err := os.RemoveAll(filepath.Join(x.opts.Source, "services", "pic-sure-gateway")); err != nil {
		t.Fatal(err)
	}

	_, err := BuildReactor(context.Background(), x.d, x.opts)
	if err == nil || !strings.Contains(err.Error(), "predates the monorepo") {
		t.Fatalf("err = %v, want the monorepo message", err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker run *"))
}

func TestReactorBuildRemovesTheContainerWhenACopyFails(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Glob("docker cp *")).Exit(1).Stderr("Error: no space left on device")
	x.reactorOK()

	_, err := BuildReactor(context.Background(), x.d, x.opts)
	if err == nil || !strings.Contains(err.Error(), "copying") {
		t.Fatalf("err = %v, want a copy failure", err)
	}
	x.f.AssertCalled(fakerunner.Exact("docker", "rm", "-v", "-f", "ctr123"))
	x.f.AssertNotCalled(fakerunner.Glob("docker build *"))
}

func TestReactorBuildFailureOfAnImageShowsTheTailAndLog(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Glob("docker build * -t hms-dbmi/pic-sure-gateway:*")).Stderr("step 3: boom\n").Exit(1)
	x.reactorOK()
	x.opts.LogDir = t.TempDir()

	_, err := BuildReactor(context.Background(), x.d, x.opts)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(x.opts.LogDir, "pic-sure-gateway.log")) ||
		strings.Contains(err.Error(), "Alpine") {
		t.Fatalf("err = %v, want the log path and no Alpine hint", err)
	}
	tail := false
	for _, e := range x.rec.Events() {
		if l, ok := e.(events.Log); ok && l.Line == "step 3: boom" {
			tail = true
		}
	}
	if !tail {
		t.Error("the build's output tail wasn't shown")
	}
	if _, err := os.Stat(filepath.Join(x.root, "build", "0123456789ab")); err != nil {
		t.Errorf("a failed build's contexts should stay: %v", err)
	}
}

func TestReactorBuildGivesACreatedContainerTimeToStart(t *testing.T) {
	defer func(p, g time.Duration) { reactorPoll, reactorCreatedGrace = p, g }(reactorPoll, reactorCreatedGrace)
	reactorPoll, reactorCreatedGrace = time.Millisecond, 3*time.Millisecond
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Exact("docker", "container", "inspect", ReactorContainer)).Times(4).
		Stdout(`[{"Id":"stuck","State":{"Status":"created","Running":false}}]`)
	x.f.On(fakerunner.Exact("docker", "rm", "-v", "-f", "stuck"))
	x.reactorOK()

	if _, err := BuildReactor(context.Background(), x.d, x.opts); err != nil {
		t.Fatal(err)
	}
	if n := len(x.f.CallsMatching(fakerunner.Exact("docker", "container", "inspect", ReactorContainer))); n != 5 {
		t.Errorf("inspected %d times, want 4 while waiting and 1 after removing it", n)
	}
	x.f.AssertOrder(fakerunner.Exact("docker", "rm", "-v", "-f", "stuck"), fakerunner.Glob("docker run -d *"))
}

func TestReactorBuildRemovesItsContainerWhenItFailsToStart(t *testing.T) {
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Exact("docker", "container", "inspect", ReactorContainer)).Times(1).
		Exit(1).Stderr("Error: No such container: " + ReactorContainer)
	x.f.On(fakerunner.Glob("docker run -d *")).Exit(125).Stderr("docker: Error response from daemon: invalid mount config")
	x.f.On(fakerunner.Exact("docker", "container", "inspect", ReactorContainer)).
		Stdout(`[{"Id":"mine","Config":{"Labels":{"` + reactorRunLabel + `":"run-72727272"}},"State":{"Status":"created"}}]`)
	x.f.On(fakerunner.Exact("docker", "rm", "-v", "-f", "mine"))
	x.reactorOK()

	_, err := BuildReactor(context.Background(), x.d, x.opts)
	if err == nil || !strings.Contains(err.Error(), "invalid mount config") {
		t.Fatalf("err = %v, want the docker error", err)
	}
	x.f.AssertCalled(fakerunner.Exact("docker", "rm", "-v", "-f", "mine"))
}

func TestReactorBuildSendsHeartbeatsWhileMavenRuns(t *testing.T) {
	defer func(e time.Duration) { heartbeatEvery = e }(heartbeatEvery)
	heartbeatEvery = time.Millisecond
	x := newReactorFixture(t)
	x.missing()
	x.f.On(fakerunner.Glob("docker exec ctr123 sh -c *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		time.Sleep(20 * time.Millisecond)
		return docker.Result{}, nil
	})
	x.f.On(fakerunner.Exact("docker", "exec", "ctr123", "touch", heartbeatFile))
	x.reactorOK()

	if _, err := BuildReactor(context.Background(), x.d, x.opts); err != nil {
		t.Fatal(err)
	}
	calls := x.f.Calls()
	beats, removed := 0, false
	for _, c := range calls {
		switch c.String() {
		case "docker exec ctr123 touch " + heartbeatFile:
			beats++
			if removed {
				t.Error("a heartbeat after the container was removed")
			}
		case "docker rm -v -f ctr123":
			removed = true
		}
	}
	if beats == 0 {
		t.Error("no heartbeats")
	}
}
