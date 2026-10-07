package ops

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// TestReactorBuildForReal builds the pic-sure images from the commit in
// PICSURE_REACTOR_SHA, in the user's real cache, and then checks that a
// second build does nothing. It takes many minutes, so it runs only when
// asked: PICSURE_REACTOR_SHA=<sha> go test -run ForReal -timeout 2h ./internal/ops
// PICSURE_REACTOR_FORCE=1 rebuilds images that are already up to date.
func TestReactorBuildForReal(t *testing.T) {
	sha := os.Getenv("PICSURE_REACTOR_SHA")
	if sha == "" {
		t.Skip("PICSURE_REACTOR_SHA not set")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no Docker daemon")
	}
	root, err := cache.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	r := &docker.ExecRunner{}
	c, err := cache.Open(root, cache.Options{Git: git.New(r), Holder: "reactor integration test"})
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	d := &Deps{Runner: r, Docker: docker.NewEngine(r), Rand: rand.Reader, Sink: events.NewPlain(os.Stderr, events.PlainOptions{Now: time.Now})}
	opts := ReactorOptions{Cache: c, SHA: sha, LogDir: logDir, Step: "build-pic-sure"}

	first := opts
	first.Force = os.Getenv("PICSURE_REACTOR_FORCE") == "1"
	start := time.Now()
	res, err := BuildReactor(context.Background(), d, first)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first build: %s, built %d images at %s", time.Since(start).Round(time.Second), len(res.Built), res.Tag)

	start = time.Now()
	res, err = BuildReactor(context.Background(), d, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Built) != 0 {
		t.Errorf("second build built %v, want nothing", res.Built)
	}
	t.Logf("second build: %s", time.Since(start).Round(time.Millisecond))
}
