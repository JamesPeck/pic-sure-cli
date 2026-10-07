package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/cache"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// execRunner is a bare-bones docker.Runner for the integration test.
type execRunner struct{}

func (r execRunner) Run(ctx context.Context, c docker.Cmd) (docker.Result, error) {
	var stdout, stderr bytes.Buffer
	code, err := r.Stream(ctx, c, &stdout, &stderr)
	return docker.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, err
}

func (execRunner) Stream(ctx context.Context, c docker.Cmd, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Dir, c.Stdin, stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// TestReactorBuildForReal builds the pic-sure images from the commit in
// PICSURE_REACTOR_SHA, in the user's real cache, and then checks that a
// second build does nothing. It takes many minutes, so it runs only when
// asked: PICSURE_REACTOR_SHA=<sha> go test -run ForReal -timeout 2h ./internal/ops
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
	r := execRunner{}
	c, err := cache.Open(root, cache.Options{Git: git.New(r), Holder: "reactor integration test"})
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	d := &Deps{Runner: r, Docker: docker.NewEngine(r), Sink: events.NewPlain(os.Stderr, events.PlainOptions{Now: time.Now})}
	opts := ReactorOptions{Cache: c, SHA: sha, LogDir: logDir, Step: "build-pic-sure"}

	start := time.Now()
	res, err := BuildReactor(context.Background(), d, opts)
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
