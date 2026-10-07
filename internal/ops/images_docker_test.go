package ops_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// TestImageBuildsWithDocker builds a stand-in frontend whose Dockerfile
// keeps the generated .env, checks the image got it byte for byte, and
// checks that the second build of each image is skipped.
func TestImageBuildsWithDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("needs docker")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	ctx := context.Background()
	e := docker.NewEngine(execRunner{})
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	// The images get this tag, and cleanup removes only those.
	tag := "test-030-" + hex.EncodeToString(b)
	alpine, _ := catalog.LookupImage("alpine")
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "Dockerfile"), []byte("FROM "+alpine.Ref+"\nCOPY .env /env\n"))
	etlSrc := t.TempDir()
	writeTestFile(t, filepath.Join(etlSrc, "Dockerfile"), []byte("FROM "+alpine.Ref+"\n"))

	c, err := cache.Open(t.TempDir(), cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d := &ops.Deps{Runner: execRunner{}, Docker: e, Sink: &events.Recorder{}}
	opts := ops.ImageBuildOptions{Cache: c, SHA: feSHA, Source: src, Tag: tag, Step: "images"}
	cfg := stack.DefaultConfig()
	cfg.Frontend.Analytics.GoogleAnalyticsID = `it's $HOME`

	for _, name := range []string{"pic-sure-httpd", "dictionary-etl"} {
		t.Cleanup(func() { _ = e.RemoveImage(context.Background(), "hms-dbmi/"+name+":"+tag) })
	}
	for run, wantBuilt := range []bool{true, false} {
		res, err := ops.BuildFrontend(ctx, d, &cfg, opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Built != wantBuilt {
			t.Errorf("frontend run %d: built %v", run+1, res.Built)
		}
		etlOpts := opts
		etlOpts.SHA, etlOpts.Source = etlSHA, etlSrc
		res, err = ops.BuildDictionaryETL(ctx, d, etlOpts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Built != wantBuilt {
			t.Errorf("dictionary-etl run %d: built %v", run+1, res.Built)
		}
	}

	var stdout bytes.Buffer
	code, err := e.Run(ctx, docker.RunOpts{
		Image: "hms-dbmi/pic-sure-httpd:" + tag, Remove: true, Network: "none",
		Args: []string{"cat", "/env"}, Stdout: &stdout,
	})
	if err != nil || code != 0 {
		t.Fatalf("cat /env: exit %d, %v", code, err)
	}
	want, _ := ops.FrontendDotEnv(render.ViteEnv(&cfg))
	if stdout.String() != string(want) {
		t.Errorf("image .env:\n%s\nwant:\n%s", stdout.String(), want)
	}
}

// TestRealImageBuilds builds the real pic-sure-httpd and dictionary-etl
// images from main, into the user's cache, for an open-mode stack, and
// checks the second run is a no-op. It runs only with
// PICSURE_REAL_IMAGE_BUILD=1 and keeps the images.
func TestRealImageBuilds(t *testing.T) {
	if os.Getenv("PICSURE_REAL_IMAGE_BUILD") != "1" {
		t.Skip("set PICSURE_REAL_IMAGE_BUILD=1")
	}
	ctx := context.Background()
	root, err := cache.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(root, cache.Options{Git: git.New(execRunner{}), Holder: "ops test 030"})
	if err != nil {
		t.Fatal(err)
	}
	d := &ops.Deps{Runner: execRunner{}, Docker: docker.NewEngine(execRunner{}), Sink: &events.Recorder{}}
	cfg := stack.DefaultConfig()
	cfg.Auth.Mode = stack.AuthOpen
	logs := t.TempDir()

	build := func(component string, f func(ops.ImageBuildOptions) (ops.ImageBuildResult, error)) {
		sha, err := c.ResolveRef(ctx, component, "main")
		if err != nil {
			t.Fatal(err)
		}
		opts := ops.ImageBuildOptions{Cache: c, SHA: sha, LogDir: logs, Step: "images"}
		for run, wantBuilt := range []bool{true, false} {
			start := time.Now()
			res, err := f(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s run %d: %s built=%v in %s", component, run+1, res.Ref, res.Built, time.Since(start).Round(time.Second))
			if run == 1 && res.Built != wantBuilt {
				t.Errorf("%s: second run built again", component)
			}
		}
	}
	build(catalog.Frontend, func(o ops.ImageBuildOptions) (ops.ImageBuildResult, error) {
		return ops.BuildFrontend(ctx, d, &cfg, o)
	})
	build(catalog.DictionaryETL, func(o ops.ImageBuildOptions) (ops.ImageBuildResult, error) {
		return ops.BuildDictionaryETL(ctx, d, o)
	})
}
