package docker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// foregroundHelperEnv makes TestForegroundHelper run: it is the process a
// pty test starts as the terminal's session leader.
const foregroundHelperEnv = "PICSURE_TEST_FOREGROUND_HELPER"

// TestForegroundHelper reads a line from the terminal through a child the
// runner starts, with Foreground set as the env var says, and reports how
// the child ended.
func TestForegroundHelper(t *testing.T) {
	mode := os.Getenv(foregroundHelperEnv)
	if mode == "" {
		t.Skip("run by TestExecForegroundReadsTheTerminal")
	}
	r := &docker.ExecRunner{Foreground: mode == "foreground", WaitDelay: 100 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fmt.Println("ready")
	cmd := sh(`read line && echo "got:$line"`)
	cmd.Stdin = os.Stdin
	code, err := r.Stream(ctx, cmd, os.Stdout, os.Stderr)
	fmt.Printf("exit:%d timedout:%t\n", code, errors.Is(err, context.DeadlineExceeded))
}

// A child that reads the terminal works only in the foreground: in its own
// process group it is stopped by SIGTTIN until ctx ends.
func TestExecForegroundReadsTheTerminal(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		mode, want string
	}{
		{"foreground", "got:hello\r\nexit:0 timedout:false"},
		{"background", "timedout:true"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(os.Args[0], "-test.run=^TestForegroundHelper$")
			cmd.Env = append(os.Environ(), foregroundHelperEnv+"="+tt.mode)
			term, err := pty.Start(cmd) // the helper leads a new session on the pty
			if err != nil {
				t.Fatal(err)
			}
			// Close the pty before waiting: the helper's exit can block until
			// its terminal output is read.
			defer func() { _ = cmd.Process.Kill(); _ = term.Close(); _ = cmd.Wait() }()

			out := make(chan string)
			go func() {
				var b bytes.Buffer
				buf := make([]byte, 1024)
				sent := false
				for {
					n, err := term.Read(buf)
					b.Write(buf[:n])
					if !sent && strings.Contains(b.String(), "ready") {
						sent = true
						_, _ = term.Write([]byte("hello\n"))
					}
					if strings.Contains(b.String(), "timedout:") || err != nil {
						out <- b.String()
						return
					}
				}
			}()
			select {
			case got := <-out:
				if !strings.Contains(got, tt.want) {
					t.Errorf("terminal output %q, want it to contain %q", got, tt.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the helper didn't finish")
			}
		})
	}
}

// When the CLI got SIGINT, the terminal sent it to the foreground child
// too, so the runner doesn't signal the child again; on any other
// cancellation, the child gets SIGTERM. Either way the call waits for the
// child, then returns the context's error.
func TestExecForegroundCancel(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		cause error
		want  int
	}{
		{"SIGINT", exitcode.Signaled(os.Interrupt), 7},
		{"SIGTERM", exitcode.Signaled(syscall.SIGTERM), 9},
		{"no cause", nil, 9},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			r := &docker.ExecRunner{Foreground: true, WaitDelay: time.Minute}
			stdout := writerFunc(func(p []byte) (int, error) {
				if strings.Contains(string(p), "ready") {
					cancel(tt.cause)
				}
				return len(p), nil
			})

			code, err := r.Stream(ctx, sh(`trap 'exit 9' TERM; echo ready; sleep 0.3; exit 7`), stdout, nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if code != tt.want {
				t.Errorf("code %d, want %d", code, tt.want)
			}
		})
	}
}
