package smoke

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The built binary writing into a pipe whose reader has gone, as with
// `pic-sure up --json | head -n1`, cancels the run and exits 141 instead of
// dying of SIGPIPE mid-step.
func TestClosedPipeExits141(t *testing.T) {
	for _, tt := range []struct {
		name   string
		args   []string
		stderr bool // the closed pipe is stderr, not stdout
	}{
		{"stdout under --json", []string{"--json"}, false},
		{"stderr in plain mode", []string{"--plain"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			var other bytes.Buffer
			cmd := exec.Command(bin, append(tt.args, "smoke-steps", "--wait")...)
			if tt.stderr {
				cmd.Stdout, cmd.Stderr = &other, w
			} else {
				cmd.Stdout, cmd.Stderr = w, &other
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = w.Close()
			// Read one line, like head -n1, then go away while the
			// command still has output to write.
			if _, err := bufio.NewReader(r).ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			_ = r.Close()

			err = waitWithin(t, cmd, 20*time.Second)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 141 {
				t.Fatalf("exit: %v (want status 141, not a signal); other stream:\n%s", err, other.String())
			}
			if tt.stderr {
				if other.Len() != 0 {
					t.Errorf("stdout = %q", other.String())
				}
			} else if got := other.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "interrupted (broken pipe)") {
				t.Errorf("stderr = %q", got)
			}
		})
	}
}

// SIGPIPE from elsewhere, such as a subprocess's stdin closing early, must
// not cancel the run: only a write to stdout or stderr does. This guards
// on Linux only: on macOS the Go runtime drops a SIGPIPE it didn't raise
// itself (golang.org/issue/33384), whatever main asks for.
func TestStraySIGPIPEDoesNotCancel(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	cmd := exec.Command(bin, "--plain", "smoke-steps", "--wait")
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	stderr := bufio.NewReader(r)
	for {
		line, err := stderr.ReadString('\n')
		if err != nil {
			t.Fatalf("no wait for cancellation: %v", err)
		}
		if strings.Contains(line, "waiting for cancellation") {
			break
		}
	}
	if err := cmd.Process.Signal(syscall.SIGPIPE); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	var exitErr *exec.ExitError
	if err := waitWithin(t, cmd, 20*time.Second); !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("exit: %v, want status 143 from the SIGTERM", err)
	}
}

// waitWithin waits for cmd, killing it and failing the test if it takes
// longer than d.
func waitWithin(t *testing.T, cmd *exec.Cmd, d time.Duration) error {
	t.Helper()
	timer := time.AfterFunc(d, func() { _ = cmd.Process.Kill() })
	err := cmd.Wait()
	if !timer.Stop() {
		t.Fatalf("still running after %v", d)
	}
	return err
}
