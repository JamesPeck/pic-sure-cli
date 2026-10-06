package docker_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

const etlStarted = "2026-10-06 20:21:00 INFO  Started DictionaryEtlApplication in 9.1 seconds\n"

// blockUntilCancelled is `docker logs -f` on a container that keeps running
// without logging anything more.
func blockUntilCancelled(ctx context.Context, _ fakerunner.Call) (docker.Result, error) {
	<-ctx.Done()
	return docker.Result{ExitCode: 143}, ctx.Err()
}

func TestLogs(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "logs", "demo-dictionaryetl")).Stdout("out line\n").Stderr("err line\n")

	logs := e.Logs(context.Background(), "demo-dictionaryetl", false)
	b, err := io.ReadAll(logs)
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if got := string(b); !strings.Contains(got, "out line\n") || !strings.Contains(got, "err line\n") {
		t.Errorf("logs = %q, want stdout and stderr merged", got)
	}
}

func TestLogsFailure(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "logs", "-f", "gone")).Stderr("Error response from daemon: No such container: gone\n").Exit(1)

	logs := e.Logs(context.Background(), "gone", true)
	defer func() { _ = logs.Close() }()
	_, err := io.ReadAll(logs)
	if !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound from Read", err)
	}
}

func TestLogsCloseStopsFollowing(t *testing.T) {
	f, e := newEngine(t)
	started, stopped := make(chan struct{}), make(chan struct{})
	f.On(fakerunner.Exact("docker", "logs", "-f", "c")).Do(func(ctx context.Context, c fakerunner.Call) (docker.Result, error) {
		defer close(stopped)
		close(started)
		return blockUntilCancelled(ctx, c)
	})

	logs := e.Logs(context.Background(), "c", true)
	<-started
	if err := logs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Error("Close returned before docker logs stopped")
	}
}

func TestWaitForLogLine(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "logs", "-f", "demo-dictionaryetl")).
		Stdout("Starting DictionaryEtlApplication\n" + etlStarted + "more output\n")

	if err := e.WaitForLogLine(context.Background(), "demo-dictionaryetl", "Started DictionaryEtlApplication", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForLogLineOnAVeryLongLine(t *testing.T) {
	f, e := newEngine(t)
	// One line longer than bufio.Scanner's limit.
	long := strings.Repeat("x", 100<<10)
	f.On(fakerunner.Glob("docker logs -f *")).Stdout(long + strings.TrimSuffix(etlStarted, "\n"))

	if err := e.WaitForLogLine(context.Background(), "c", "Started DictionaryEtlApplication", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForLogLineContainerStops(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker logs -f *")).Stdout("APPLICATION FAILED TO START\n")

	err := e.WaitForLogLine(context.Background(), "demo-dictionaryetl", "Started DictionaryEtlApplication", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "stopped without logging") {
		t.Errorf("err = %v", err)
	}
}

func TestWaitForLogLineTimeout(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker logs -f *")).Do(blockUntilCancelled)

	err := e.WaitForLogLine(context.Background(), "demo-dictionaryetl", "Started", 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `did not log "Started" within 20ms`) {
		t.Errorf("err = %v", err)
	}
}

func TestWaitForLogLineCancelled(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker logs -f *")).Do(blockUntilCancelled)
	cause := errors.New("interrupted")
	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(10*time.Millisecond, func() { cancel(cause) })

	if err := e.WaitForLogLine(ctx, "c", "Started", time.Minute); !errors.Is(err, cause) {
		t.Errorf("err = %v, want the context's cause", err)
	}
}

func TestWaitForLogLineDockerFails(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker logs -f *")).Stderr("Error response from daemon: No such container: gone\n").Exit(1)

	if err := e.WaitForLogLine(context.Background(), "gone", "Started", time.Minute); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestWaitForLogLineEmptySubstring(t *testing.T) {
	_, e := newEngine(t)
	if err := e.WaitForLogLine(context.Background(), "c", "", time.Minute); err == nil {
		t.Error("no error")
	}
}
