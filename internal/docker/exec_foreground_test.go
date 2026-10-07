package docker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
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

// A foreground child gets no signal when ctx ends (Ctrl-C has reached it
// from the terminal already): the call waits for it, then returns the
// context's error.
func TestExecForegroundCancelLeavesTheChildAlone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &docker.ExecRunner{Foreground: true}
	stdout := writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "ready") {
			cancel()
		}
		return len(p), nil
	})

	code, err := r.Stream(ctx, sh(`trap 'exit 1' TERM; echo ready; sleep 0.3; exit 7`), stdout, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if code != 7 {
		t.Errorf("code %d, want 7: the child was signalled", code)
	}
}
