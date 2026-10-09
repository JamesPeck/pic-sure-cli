package ops_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const (
	feSHA  = "0123456789abcdef0123456789abcdef01234567"
	etlSHA = "fedcba9876543210fedcba9876543210fedcba98"
)

func TestFrontendConfigHashIsStable(t *testing.T) {
	a := map[string]string{"VITE_A": "1", "VITE_B": "2", "VITE_THEME": "picsure"}
	b := map[string]string{}
	for _, k := range []string{"VITE_THEME", "VITE_B", "VITE_A"} {
		b[k] = a[k]
	}
	if ops.FrontendConfigHash(a) != ops.FrontendConfigHash(b) {
		t.Error("the hash depends on map order")
	}
	// Changing the canonical form re-tags every frontend image, so it is
	// pinned here.
	const want = "a00ddcf96616abd92058b9b39b87f6f84ebbc1db3cac008230815472f9fa50a1"
	if got := ops.FrontendConfigHash(a); got != want {
		t.Errorf("FrontendConfigHash = %s, want %s", got, want)
	}
}

func TestFrontendConfigHashIsSensitive(t *testing.T) {
	base := map[string]string{"VITE_A": "1", "VITE_B": "2"}
	variants := map[string]map[string]string{
		"value":        {"VITE_A": "1", "VITE_B": "3"},
		"name":         {"VITE_A": "1", "VITE_C": "2"},
		"added":        {"VITE_A": "1", "VITE_B": "2", "VITE_C": ""},
		"removed":      {"VITE_A": "1"},
		"moved across": {"VITE_A": "1\"VITE_B\":\"2", "VITE_B": ""},
	}
	h := ops.FrontendConfigHash(base)
	for name, v := range variants {
		if ops.FrontendConfigHash(v) == h {
			t.Errorf("%s: hash unchanged", name)
		}
	}

	cfg := stack.DefaultConfig()
	h = ops.FrontendConfigHash(render.ViteEnv(&cfg))
	for name, change := range map[string]func(*stack.Config){
		"theme":     func(c *stack.Config) { c.Frontend.Theme = "bdc" },
		"auth mode": func(c *stack.Config) { c.Auth.Mode = stack.AuthOpen },
		"client id": func(c *stack.Config) { c.Auth.Auth0.ClientID = "abc" },
		"analytics": func(c *stack.Config) { c.Frontend.Analytics.GoogleAnalyticsID = "G-1" },
	} {
		c := stack.DefaultConfig()
		change(&c)
		if ops.FrontendConfigHash(render.ViteEnv(&c)) == h {
			t.Errorf("%s: hash unchanged", name)
		}
	}
}

// The expected forms were checked against Vite 8's loadEnv, which reads the
// file with dotenv and dotenv-expand: each value comes back unchanged.
func TestFrontendDotEnv(t *testing.T) {
	got, err := ops.FrontendDotEnv(map[string]string{
		"VITE_THEME":  "picsure",
		"VITE_DOLLAR": `a $HOME ${X} \$b $`,
		"VITE_QUOTE":  `it's "x" $Y`,
		"VITE_HASH":   "x # y",
		"VITE_EMPTY":  "",
		"VITE_BACK":   `ends\`,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `VITE_BACK='ends\'
VITE_DOLLAR='a \$HOME \${X} \\$b \$'
VITE_EMPTY=''
VITE_HASH='x # y'
VITE_QUOTE=` + "`it's \"x\" \\$Y`" + `
VITE_THEME='picsure'
`
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	for name, env := range map[string]map[string]string{
		"line feed":   {"VITE_A": "a\nb"},
		"return":      {"VITE_A": "a\rb"},
		"both quotes": {"VITE_A": "it's `x`"},
		"not VITE":    {"AUTH0_SECRET": "x"},
		"lower case":  {"VITE_a": "x"},
	} {
		if _, err := ops.FrontendDotEnv(env); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

type imageFixture struct {
	f     *fakerunner.Runner
	d     *ops.Deps
	rec   *events.Recorder
	cache *cache.Cache
	src   string
}

func newImageFixture(t *testing.T) *imageFixture {
	t.Helper()
	c, err := cache.Open(t.TempDir(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Resolved, as the build resolves its source (/var is a link on macOS).
	src, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"Dockerfile":         "FROM scratch\n",
		"src/routes/page.ts": "export {}\n",
		".env.example":       "VITE_X=1\n",
		".git/HEAD":          "ref: refs/heads/main\n",
		"node_modules/x.js":  "x\n",
	} {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("routes/page.ts", filepath.Join(src, "src", "link.ts")); err != nil {
		t.Fatal(err)
	}
	f := fakerunner.New(t)
	rec := &events.Recorder{}
	return &imageFixture{
		f:     f,
		d:     &ops.Deps{Runner: f, Docker: docker.NewEngine(f), Sink: rec},
		rec:   rec,
		cache: c,
		src:   src,
	}
}

func (x *imageFixture) opts(sha string) ops.ImageBuildOptions {
	return ops.ImageBuildOptions{Cache: x.cache, SHA: sha, Source: x.src, Step: "images"}
}

func (x *imageFixture) missing() {
	x.f.On(fakerunner.Glob("docker image inspect *")).Exit(1).
		Stderr("Error response from daemon: No such image: x\n")
}

func (x *imageFixture) labelled(labels string) {
	x.f.On(fakerunner.Glob("docker image inspect *")).
		Stdout(`[{"Id":"sha256:1","Config":{"Labels":` + labels + `}}]`)
}

func TestBuildFrontendCopiesTheSourceAndWritesTheEnv(t *testing.T) {
	x := newImageFixture(t)
	cfg := stack.DefaultConfig()
	cfg.Frontend.Theme = "bdc"
	env := render.ViteEnv(&cfg)
	hash := ops.FrontendConfigHash(env)
	tag := feSHA[:12] + "-" + hash[:8]
	ctxDir, err := x.cache.FrontendBuildDir(tag)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv, _ := ops.FrontendDotEnv(env)

	x.missing()
	var sawContext bool
	x.f.On(fakerunner.Glob("docker build *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		got, err := os.ReadFile(filepath.Join(ctxDir, ".env"))
		if err != nil || string(got) != string(wantEnv) {
			t.Errorf(".env in the context: %q, %v; want %q", got, err, wantEnv)
		}
		if !strings.Contains(string(got), "VITE_ORIGIN='http://localhost'\n") {
			t.Errorf(".env has no VITE_ORIGIN for SSR config fetches: %q", got)
		}
		if link, err := os.Readlink(filepath.Join(ctxDir, "src", "link.ts")); err != nil || link != "routes/page.ts" {
			t.Errorf("symlink copied as %q, %v", link, err)
		}
		if _, err := os.Stat(filepath.Join(ctxDir, "src", "routes", "page.ts")); err != nil {
			t.Error(err)
		}
		for _, skipped := range []string{".git", "node_modules"} {
			if _, err := os.Lstat(filepath.Join(ctxDir, skipped)); !os.IsNotExist(err) {
				t.Errorf("%s copied into the context (%v)", skipped, err)
			}
		}
		sawContext = true
		return docker.Result{}, nil
	})

	opts := x.opts(feSHA)
	opts.Proxy, err = netproxy.New(netproxy.Config{HTTPS: "http://user:pw@proxy:3128"}, netproxy.CatalogServices())
	if err != nil {
		t.Fatal(err)
	}
	res, err := ops.BuildFrontend(context.Background(), x.d, &cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !sawContext {
		t.Fatal("docker build not run")
	}
	want := ops.ImageBuildResult{Tag: tag, Ref: "hms-dbmi/pic-sure-httpd:" + tag, Built: true}
	if res != want {
		t.Errorf("result %+v, want %+v", res, want)
	}
	x.f.AssertCalled(fakerunner.Exact("docker", "build",
		"-f", filepath.Join(ctxDir, "Dockerfile"),
		"-t", want.Ref,
		"--label", ops.FrontendConfigLabel+"="+hash,
		"--label", ops.FrontendSrcLabel+"="+feSHA,
		"--build-arg", "HTTP_PROXY", "--build-arg", "http_proxy",
		"--build-arg", "HTTPS_PROXY", "--build-arg", "https_proxy",
		"--build-arg", "ALL_PROXY", "--build-arg", "all_proxy",
		"--build-arg", "NO_PROXY", "--build-arg", "no_proxy",
		ctxDir))
	build := x.f.CallsMatching(fakerunner.Glob("docker build *"))[0]
	if !build.HasEnv("HTTPS_PROXY") || strings.Contains(build.String(), "pw@") {
		t.Errorf("proxy build args: argv %s, env %v", build, build.Env)
	}
	if _, err := os.Stat(ctxDir); !os.IsNotExist(err) {
		t.Errorf("build context left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.src, ".env")); !os.IsNotExist(err) {
		t.Errorf("source tree got a .env: %v", err)
	}
}

// The generated .env replaces a copied one without writing through it,
// and a symlinked source root is copied as the tree it points to.
func TestBuildFrontendLeavesTheSourceAlone(t *testing.T) {
	x := newImageFixture(t)
	outside := filepath.Join(t.TempDir(), "developer.env")
	writeTestFile(t, outside, []byte("VITE_MINE=1\n"))
	if err := os.Symlink(outside, filepath.Join(x.src, ".env")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "frontend")
	if err := os.Symlink(x.src, link); err != nil {
		t.Fatal(err)
	}
	opts := x.opts(feSHA)
	opts.Source, opts.Tag = link, "dev-a-0123456789ab"
	ctxDir, err := x.cache.FrontendBuildDir(opts.Tag)
	if err != nil {
		t.Fatal(err)
	}
	cfg := stack.DefaultConfig()
	wantEnv, _ := ops.FrontendDotEnv(render.ViteEnv(&cfg))

	x.missing()
	x.f.On(fakerunner.Glob("docker build *")).Do(func(context.Context, fakerunner.Call) (docker.Result, error) {
		if fi, err := os.Lstat(filepath.Join(ctxDir, ".env")); err != nil || !fi.Mode().IsRegular() {
			t.Errorf(".env in the context: %v, %v", fi, err)
		}
		if got, _ := os.ReadFile(filepath.Join(ctxDir, ".env")); string(got) != string(wantEnv) {
			t.Errorf(".env in the context: %q", got)
		}
		if _, err := os.Stat(filepath.Join(ctxDir, "src", "routes", "page.ts")); err != nil {
			t.Error(err)
		}
		return docker.Result{}, nil
	})
	if _, err := ops.BuildFrontend(context.Background(), x.d, &cfg, opts); err != nil {
		t.Fatal(err)
	}
	x.f.AssertCalled(fakerunner.Glob("docker build * -t hms-dbmi/pic-sure-httpd:dev-a-0123456789ab * " + ctxDir))
	if got, _ := os.ReadFile(outside); string(got) != "VITE_MINE=1\n" {
		t.Errorf("the developer's .env became %q", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("source link: %v, %v", fi, err)
	}
}

func TestBuildFrontendSkipsAnUpToDateImage(t *testing.T) {
	x := newImageFixture(t)
	cfg := stack.DefaultConfig()
	hash := ops.FrontendConfigHash(render.ViteEnv(&cfg))
	x.labelled(`{"` + ops.FrontendSrcLabel + `":"` + feSHA + `","` + ops.FrontendConfigLabel + `":"` + hash + `"}`)

	res, err := ops.BuildFrontend(context.Background(), x.d, &cfg, x.opts(feSHA))
	if err != nil {
		t.Fatal(err)
	}
	if res.Built || res.Tag != feSHA[:12]+"-"+hash[:8] {
		t.Errorf("result %+v", res)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker build *"))
}

func TestBuildFrontendRebuildsOnALabelMismatch(t *testing.T) {
	for name, labels := range map[string]string{
		"no labels":      `{}`,
		"other config":   `{"` + ops.FrontendSrcLabel + `":"` + feSHA + `","` + ops.FrontendConfigLabel + `":"0000"}`,
		"other commit":   `{"` + ops.FrontendSrcLabel + `":"` + etlSHA + `"}`,
		"labels is null": `null`,
	} {
		t.Run(name, func(t *testing.T) {
			x := newImageFixture(t)
			x.labelled(labels)
			x.f.On(fakerunner.Glob("docker build *"))
			cfg := stack.DefaultConfig()
			res, err := ops.BuildFrontend(context.Background(), x.d, &cfg, x.opts(feSHA))
			if err != nil || !res.Built {
				t.Errorf("result %+v, %v", res, err)
			}
		})
	}
}

func TestBuildFrontendForceRebuilds(t *testing.T) {
	x := newImageFixture(t)
	x.f.On(fakerunner.Glob("docker build *"))
	cfg := stack.DefaultConfig()
	opts := x.opts(feSHA)
	opts.Force = true
	if res, err := ops.BuildFrontend(context.Background(), x.d, &cfg, opts); err != nil || !res.Built {
		t.Errorf("result %+v, %v", res, err)
	}
	x.f.AssertNotCalled(fakerunner.Glob("docker image inspect *"))
}

func TestBuildFrontendRefusesAnUnwritableEnv(t *testing.T) {
	x := newImageFixture(t)
	cfg := stack.DefaultConfig()
	cfg.Frontend.Analytics.GoogleAnalyticsID = "G-1\nVITE_EVIL=1"
	if _, err := ops.BuildFrontend(context.Background(), x.d, &cfg, x.opts(feSHA)); err == nil {
		t.Fatal("no error")
	}
	if calls := x.f.Calls(); len(calls) != 0 {
		t.Errorf("ran %v", calls)
	}
}

func TestBuildDictionaryETL(t *testing.T) {
	x := newImageFixture(t)
	x.missing()
	x.f.On(fakerunner.Glob("docker build *"))
	res, err := ops.BuildDictionaryETL(context.Background(), x.d, x.opts(etlSHA))
	if err != nil {
		t.Fatal(err)
	}
	ref := "hms-dbmi/dictionary-etl:" + etlSHA[:12]
	if res != (ops.ImageBuildResult{Tag: etlSHA[:12], Ref: ref, Built: true}) {
		t.Errorf("result %+v", res)
	}
	x.f.AssertCalled(fakerunner.Exact("docker", "build",
		"-f", filepath.Join(x.src, "Dockerfile"),
		"-t", ref,
		"--label", ops.DictionaryETLSrcLabel+"="+etlSHA,
		x.src))

	x = newImageFixture(t)
	x.labelled(`{"` + ops.DictionaryETLSrcLabel + `":"` + etlSHA + `"}`)
	if res, err := ops.BuildDictionaryETL(context.Background(), x.d, x.opts(etlSHA)); err != nil || res.Built {
		t.Errorf("second run: %+v, %v", res, err)
	}
}

func TestImageBuildFailureShowsTheTailAndTheLog(t *testing.T) {
	x := newImageFixture(t)
	x.missing()
	x.f.On(fakerunner.Glob("docker build *")).Exit(1).Stderr("step 1\nERROR: pnpm build failed\n")
	opts := x.opts(etlSHA)
	opts.LogDir = t.TempDir()
	_, err := ops.BuildDictionaryETL(context.Background(), x.d, opts)
	logPath := filepath.Join(opts.LogDir, "dictionary-etl.log")
	if err == nil || !strings.Contains(err.Error(), "full log: "+logPath) {
		t.Fatalf("error %v", err)
	}
	if b, _ := os.ReadFile(logPath); !strings.Contains(string(b), "pnpm build failed") {
		t.Errorf("log %q", b)
	}
	var tail []string
	for _, e := range x.rec.Events() {
		if l, ok := e.(events.Log); ok {
			tail = append(tail, l.Line)
		}
	}
	if strings.Join(tail, "|") != "step 1|ERROR: pnpm build failed" {
		t.Errorf("tail %q", tail)
	}
}

func TestImageBuildRefusesAShortSHA(t *testing.T) {
	x := newImageFixture(t)
	cfg := stack.DefaultConfig()
	if _, err := ops.BuildFrontend(context.Background(), x.d, &cfg, x.opts("0123456789ab")); err == nil {
		t.Error("frontend: no error")
	}
	if _, err := ops.BuildDictionaryETL(context.Background(), x.d, x.opts("main")); err == nil {
		t.Error("dictionary-etl: no error")
	}
}
