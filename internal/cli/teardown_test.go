package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func TestConfirmName(t *testing.T) {
	cmd := &cobra.Command{Use: "destroy"}
	cmd.SetContext(context.Background())
	for _, tc := range []struct {
		name             string
		terminal         bool
		stderrRedirected bool
		yes              bool
		input            string
		want             int
	}{
		{"typed the name", true, false, false, "demo\n", exitcode.CodeOK},
		{"typed without a newline", true, false, false, "demo", exitcode.CodeOK},
		{"typed something else", true, false, false, "dem\n", exitcode.CodeConfirmRequired},
		{"typed nothing", true, false, false, "", exitcode.CodeConfirmRequired},
		{"no terminal", false, false, false, "demo\n", exitcode.CodeConfirmRequired},
		{"stderr redirected", true, true, false, "demo\n", exitcode.CodeConfirmRequired},
		{"--yes", false, false, true, "", exitcode.CodeOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, stderr := testApp(t)
			a.stdinTerminal = func() bool { return tc.terminal }
			a.stderrTerminal = func() bool { return tc.terminal && !tc.stderrRedirected }
			a.Global.Yes = tc.yes
			a.Stdin = strings.NewReader(tc.input)
			err := a.confirmName(cmd, "demo", "This removes stack demo.")
			if got := exitcode.FromError(err); got != tc.want {
				t.Errorf("exit = %d (%v), want %d", got, err, tc.want)
			}
			asked := strings.Contains(stderr.String(), "Type the stack name (demo) to confirm: ")
			if asked != (tc.terminal && !tc.stderrRedirected && !tc.yes) {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}

func TestConfirmYes(t *testing.T) {
	cmd := &cobra.Command{Use: "rotate"}
	cmd.SetContext(context.Background())
	for _, tc := range []struct {
		name             string
		stderrRedirected bool
		input            string
		want             int
	}{
		{"y", false, "y\n", exitcode.CodeOK},
		{"no", false, "n\n", exitcode.CodeConfirmRequired},
		{"stderr redirected", true, "y\n", exitcode.CodeConfirmRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, stderr := testApp(t)
			a.stdinTerminal = func() bool { return true }
			a.stderrTerminal = func() bool { return !tc.stderrRedirected }
			a.Stdin = strings.NewReader(tc.input)
			err := a.confirmYes(cmd, "This replaces it.")
			if got := exitcode.FromError(err); got != tc.want {
				t.Errorf("exit = %d (%v), want %d", got, err, tc.want)
			}
			if asked := strings.Contains(stderr.String(), "This replaces it.\nContinue? [y/N] "); asked == tc.stderrRedirected {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}

// A first Ctrl-C at the prompt ends destroy at once, with exit 130 and the
// stack untouched.
func TestCtrlCAtTheConfirmation(t *testing.T) {
	dir := gateStack(t, stack.ConfigSchema, "")
	a, _, _ := testApp(t)
	stderr := &lockedBuffer{}
	a.Stderr = stderr
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	a.Stdin = pr
	a.stdinTerminal = func() bool { return true }
	a.stderrTerminal = func() bool { return true }
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	code := make(chan int, 1)
	go func() { code <- a.Run(ctx, []string{"destroy", "--stack", dir}) }()
	waitFor(t, stderr, "to confirm: ")
	cancel(exitcode.Signaled(os.Interrupt))
	select {
	case got := <-code:
		if got != exitcode.CodeInterrupted {
			t.Errorf("exit = %d, want %d; stderr %q", got, exitcode.CodeInterrupted, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("destroy kept waiting for an answer")
	}
	if _, err := os.Stat(filepath.Join(dir, stack.ConfigFile)); err != nil {
		t.Errorf("the stack was changed: %v", err)
	}
}
