package docker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

// sh is a Cmd that runs script with sh, with args as $1...
func sh(script string, args ...string) docker.Cmd {
	return docker.Cmd{Argv: append([]string{"sh", "-c", script, "sh"}, args...)}
}

func TestExecExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		script string
		want   int
	}{
		{"success", "exit 0", 0},
		{"failure", "exit 3", 3},
		{"SIGTERM", "kill -TERM $$", 128 + int(syscall.SIGTERM)},
		{"SIGKILL", "kill -KILL $$", 128 + int(syscall.SIGKILL)},
		{"SIGUSR1", "kill -USR1 $$", 128 + int(syscall.SIGUSR1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &docker.ExecRunner{}
			res, err := r.Run(context.Background(), sh(tt.script))
			if err != nil || res.ExitCode != tt.want {
				t.Errorf("Run = %d, %v; want %d, nil", res.ExitCode, err, tt.want)
			}
			code, err := r.Stream(context.Background(), sh(tt.script), nil, nil)
			if err != nil || code != tt.want {
				t.Errorf("Stream = %d, %v; want %d, nil", code, err, tt.want)
			}
		})
	}
}

func TestExecRunCapturesOutputAndTakesStdinAndDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := sh(`cat; echo "to stderr" >&2; pwd -P`)
	c.Stdin = strings.NewReader("from stdin\n")
	c.Dir = dir

	res, err := (&docker.ExecRunner{}).Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "from stdin\n" + resolved + "\n"; string(res.Stdout) != want {
		t.Errorf("stdout = %q, want %q", res.Stdout, want)
	}
	if string(res.Stderr) != "to stderr\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestExecStartFailure(t *testing.T) {
	t.Parallel()
	r := &docker.ExecRunner{}
	res, err := r.Run(context.Background(), docker.Cmd{Argv: []string{"pic-sure-test-no-such-program"}})
	if !errors.Is(err, exec.ErrNotFound) || res.ExitCode != -1 {
		t.Errorf("missing program: %d, %v; want -1, ErrNotFound", res.ExitCode, err)
	}
	if _, err := r.Run(context.Background(), docker.Cmd{}); err == nil {
		t.Error("empty argv: no error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Stream(ctx, sh("exit 0"), nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled before start: %v, want context.Canceled", err)
	}
}

// TestExecNotWedgedByOrphanGrandchild ports v1's poller lesson (bug B3): a
// script that backgrounds a grandchild hands it the stdout pipe. If
// cancellation killed only the script, the grandchild would hold the pipe
// open and Run would block until its 60 s sleep ended.
func TestExecNotWedgedByOrphanGrandchild(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := (&docker.ExecRunner{}).Run(ctx, sh("echo started; sleep 60 & sleep 60"))
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Run took %v after a 500ms deadline: the orphaned grandchild held stdout open", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if string(res.Stdout) != "started\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestExecCancelSignalsTheWholeGroup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A long WaitDelay: the group must go on SIGTERM alone.
	r := &docker.ExecRunner{WaitDelay: time.Minute}
	pids := make(chan int, 1)
	stdout := writerFunc(func(p []byte) (int, error) {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(p))); err == nil {
			pids <- pid
			cancel()
		}
		return len(p), nil
	})

	start := time.Now()
	_, err := r.Stream(ctx, sh("sleep 60 & echo $!; wait"), stdout, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Stream took %v to return after SIGTERM", elapsed)
	}
	assertGone(t, <-pids)
}

func TestExecCancelEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const delay = 300 * time.Millisecond
	r := &docker.ExecRunner{WaitDelay: delay}
	pids := make(chan int, 1)
	var cancelled time.Time
	stdout := writerFunc(func(p []byte) (int, error) {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(p))); err == nil {
			pids <- pid
			cancelled = time.Now()
			cancel()
		}
		return len(p), nil
	})

	// The script and its grandchild both ignore SIGTERM.
	_, err := r.Stream(ctx, sh("trap '' TERM; sleep 60 & echo $!; wait"), stdout, nil)
	elapsed := time.Since(cancelled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed < delay || elapsed > delay+5*time.Second {
		t.Errorf("returned %v after cancel, want just over the %v grace period", elapsed, delay)
	}
	assertGone(t, <-pids)
}

func TestExecCancelSendsSIGTERMFirst(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &docker.ExecRunner{WaitDelay: time.Minute}
	stdout := writerFunc(func(p []byte) (int, error) {
		if string(p) == "ready\n" {
			cancel()
		}
		return len(p), nil
	})
	var stderr bytes.Buffer

	code, err := r.Stream(ctx, sh(`trap 'echo cleaned up >&2; exit 7' TERM; echo ready; while :; do sleep 0.05; done`), stdout, &stderr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// bash may also report its foreground sleep's death on stderr.
	if code != 7 || !strings.HasSuffix(stderr.String(), "cleaned up\n") {
		t.Errorf("code %d, stderr %q: the TERM trap did not run", code, stderr.String())
	}
}

// A command that exits but leaves a background process holding its stdout
// must not wedge Run either: it returns WaitDelay after the exit, with the
// command's own result.
func TestExecLingeringGrandchildAfterExit(t *testing.T) {
	t.Parallel()
	r := &docker.ExecRunner{WaitDelay: 300 * time.Millisecond}
	start := time.Now()
	res, err := r.Run(context.Background(), sh("echo hi; sleep 60 & echo $!"))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run took %v", elapsed)
	}
	lines := strings.Fields(string(res.Stdout))
	if len(lines) == 2 {
		if pid, perr := strconv.Atoi(lines[1]); perr == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if err != nil || res.ExitCode != 0 || len(lines) != 2 || lines[0] != "hi" {
		t.Errorf("Run = %+v, %v; want hi and a pid, exit 0, nil", res, err)
	}
}

func TestExecEnvIsMinimalPlusCmdEnv(t *testing.T) {
	t.Parallel()
	r := &docker.ExecRunner{Environ: []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/home/user",
		"TERM=xterm",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"DOCKER_CONTEXT=colima",
		"XDG_CACHE_HOME=/cache",
		"HTTPS_PROXY=http://proxy:3128",
		"no_proxy=localhost",
		"COMPOSE_PROJECT_NAME=someone-elses",
		"LANG=fr_FR.UTF-8",
		"GITHUB_TOKEN=not-for-children",
		"MY_DOCKER_THING=no",
	}}
	c := docker.Cmd{
		Argv: []string{"env"},
		Env:  []string{"HOME=/override", "MYSQL_PWD=from-cmd"},
	}
	res, err := r.Run(context.Background(), c)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("env: %d, %v, %s", res.ExitCode, err, res.Stderr)
	}
	got := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	slices.Sort(got)
	want := []string{
		"DOCKER_CONTEXT=colima",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"HOME=/override",
		"HTTPS_PROXY=http://proxy:3128",
		"MYSQL_PWD=from-cmd",
		"PATH=" + os.Getenv("PATH"),
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"TERM=xterm",
		"XDG_CACHE_HOME=/cache",
		"no_proxy=localhost",
	}
	if !slices.Equal(got, want) {
		t.Errorf("child env:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	empty := &docker.ExecRunner{Environ: []string{}}
	res, err = empty.Run(context.Background(), docker.Cmd{Argv: []string{"env"}})
	if err != nil || len(res.Stdout) != 0 {
		t.Errorf("empty environment: %q, %v", res.Stdout, err)
	}
}

func TestExecStreamWritesWholeLines(t *testing.T) {
	t.Parallel()
	var busy int // detects overlapping writes; -race reports them too
	out := &recorder{busy: &busy}
	errOut := &recorder{busy: &busy}
	script := `printf a; sleep 0.1; printf 'b\nc'; sleep 0.1; printf '\n'; printf 'err\n' >&2; printf tail`

	code, err := (&docker.ExecRunner{}).Stream(context.Background(), sh(script), out, errOut)
	if err != nil || code != 0 {
		t.Fatalf("Stream = %d, %v", code, err)
	}
	if want := []string{"ab\n", "c\n", "tail"}; !slices.Equal(out.writes, want) {
		t.Errorf("stdout writes = %q, want %q", out.writes, want)
	}
	if want := []string{"err\n"}; !slices.Equal(errOut.writes, want) {
		t.Errorf("stderr writes = %q, want %q", errOut.writes, want)
	}
	if out.overlapped || errOut.overlapped {
		t.Error("the stdout and stderr writers were called concurrently")
	}
}

func TestExecStreamSplitsVeryLongLines(t *testing.T) {
	t.Parallel()
	const maxLine = 64 << 10
	var busy int
	out := &recorder{busy: &busy}
	code, err := (&docker.ExecRunner{}).Stream(context.Background(),
		sh(`head -c 200000 /dev/zero | tr '\0' a; echo`), out, nil)
	if err != nil || code != 0 {
		t.Fatalf("Stream = %d, %v", code, err)
	}
	if got := strings.Join(out.writes, ""); got != strings.Repeat("a", 200000)+"\n" {
		t.Fatalf("output corrupted: %d bytes", len(got))
	}
	if len(out.writes) < 2 {
		t.Errorf("a 200000-byte line arrived in %d write(s)", len(out.writes))
	}
	for i, w := range out.writes[:len(out.writes)-1] {
		if len(w) < maxLine {
			t.Errorf("write %d is a %d-byte piece of a long line", i, len(w))
		}
	}
}

func TestExecStreamReturnsWriterError(t *testing.T) {
	t.Parallel()
	errFull := errors.New("disk full")
	stdout := writerFunc(func([]byte) (int, error) { return 0, errFull })
	_, err := (&docker.ExecRunner{}).Stream(context.Background(), sh("echo one; echo two"), stdout, nil)
	if !errors.Is(err, errFull) {
		t.Errorf("err = %v, want %v", err, errFull)
	}
}

func TestExecLogsArgvButNotEnvValuesOrStdin(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := sh("cat >/dev/null")
	c.Env = []string{"PICSURE_TEST_SECRET=hunter2"}
	c.Stdin = strings.NewReader("stdin-secret")

	if _, err := (&docker.ExecRunner{Log: log}).Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	for _, want := range []string{"cat >/dev/null", "PICSURE_TEST_SECRET", "exit=0"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	for _, secret := range []string{"hunter2", "stdin-secret"} {
		if strings.Contains(logged, secret) {
			t.Errorf("log contains %q:\n%s", secret, logged)
		}
	}
}

func TestWithTimeout(t *testing.T) {
	t.Parallel()
	r := docker.WithTimeout(&docker.ExecRunner{}, 200*time.Millisecond)

	t.Run("fires", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		_, err := r.Run(context.Background(), sh("sleep 30"))
		var timeout *docker.TimeoutError
		if !errors.As(err, &timeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a *TimeoutError", err)
		}
		if want := `sh -c "sleep 30" sh timed out after 200ms`; err.Error() != want {
			t.Errorf("message = %q, want %q", err, want)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("took %v", elapsed)
		}
	})
	t.Run("fast command", func(t *testing.T) {
		t.Parallel()
		code, err := r.Stream(context.Background(), sh("exit 4"), nil, nil)
		if err != nil || code != 4 {
			t.Errorf("Stream = %d, %v", code, err)
		}
	})
	t.Run("caller's cancellation is not a timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := r.Run(ctx, sh("sleep 30"))
		var timeout *docker.TimeoutError
		if errors.As(err, &timeout) || !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled and no *TimeoutError", err)
		}
	})
	t.Run("any runner", func(t *testing.T) {
		t.Parallel()
		f := fakerunner.New(t)
		f.On(fakerunner.Exact("docker", "compose", "ps")).Do(func(ctx context.Context, _ fakerunner.Call) (docker.Result, error) {
			<-ctx.Done()
			return docker.Result{}, ctx.Err()
		})
		_, err := docker.WithTimeout(f, 50*time.Millisecond).Stream(context.Background(),
			docker.Cmd{Argv: []string{"docker", "compose", "ps"}}, io.Discard, io.Discard)
		var timeout *docker.TimeoutError
		if !errors.As(err, &timeout) || timeout.Timeout != 50*time.Millisecond {
			t.Errorf("err = %v, want a 50ms *TimeoutError", err)
		}
	})
}

// assertGone waits for pid to disappear. A killed orphan is a zombie until
// init reaps it, so give that a moment.
func assertGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the cancellation", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// recorder keeps each Write separately. busy is shared between recorders to
// notice overlapping calls.
type recorder struct {
	writes     []string
	busy       *int
	overlapped bool
}

func (r *recorder) Write(p []byte) (int, error) {
	*r.busy++
	if *r.busy > 1 {
		r.overlapped = true
	}
	r.writes = append(r.writes, string(p))
	*r.busy--
	return len(p), nil
}
