package smoke

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// stepsEnv keeps the TUI renderer on: CI would select plain output.
var stepsEnv = []string{"CI=", "TERM=xterm-256color"}

// waitExit requires the process to exit with code within 15s.
func (s *ptySession) waitExit(code int) {
	s.t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		got := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			got = exitErr.ExitCode()
		} else if err != nil {
			s.t.Fatal(err)
		}
		if got != code {
			s.t.Fatalf("exit code %d, want %d; output:\n%s", got, code, s.text())
		}
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
		s.t.Fatalf("did not exit within 15s; output:\n%s", s.text())
	}
	// The reader goroutine may still be draining the last frame.
	time.Sleep(200 * time.Millisecond)
}

// text is the output so far without escape sequences. Bubble Tea redraws
// only what changed, so text that was overwritten in place may not appear
// whole.
func (s *ptySession) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansi.Strip(s.output.String())
}

// requireInOrder fails unless each of want appears in text after the one
// before it.
func requireInOrder(t *testing.T, text string, want ...string) {
	t.Helper()
	rest := text
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("missing %q (in this order: %q); output:\n%s", w, want, text)
		}
		rest = rest[i+len(w):]
	}
}

// A run's steps render inline: finished steps with their marks, the
// warning and a log record above the frame, the running step's log tail,
// and the command's summary below the last frame.
func TestStepsRenderInlineUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTYEnv(t, t.TempDir(), stepsEnv, "smoke-steps", "--no-animations")
	s.waitExit(0)
	out := s.text()
	requireInOrder(t, out,
		"- Prepare the stack (skipped)",
		"a log record during fetch",
		"✓ Fetch sources", "! a warning from fetch",
		"✓ Build images", "✓ Finish",
		"smoke-steps finished")
	if !strings.Contains(out, "│ build line") {
		t.Errorf("no log tail was drawn; output:\n%s", out)
	}
	if strings.Contains(out, "[ OK ]") {
		t.Errorf("plain output on a terminal; output:\n%s", out)
	}
}

// A failed step keeps its last log lines on screen, above the error.
func TestFailedStepKeepsItsLogUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTYEnv(t, t.TempDir(), stepsEnv, "smoke-steps", "--no-animations", "--fail")
	s.waitExit(1)
	out := s.text()
	requireInOrder(t, out, "✗ Build images", "│ build line 11", "│ build line 30", "pic-sure: ", "the build failed")
}

// Ctrl-C is a key while the TUI runs: the first press asks, the second
// cancels the operation, which still finishes its step, and the CLI exits
// 130 naming the step to resume from.
func TestCtrlCCancelsStepsUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTYEnv(t, t.TempDir(), stepsEnv, "smoke-steps", "--no-animations", "--wait")
	s.waitFor("waiting for cancellation")
	s.send("\x03")
	s.waitFor("Press Ctrl-C again to cancel.")
	s.send("\x03")
	s.waitExit(130)
	requireInOrder(t, s.text(), "✗ Build images", "│ cleaning up after cancellation", "pic-sure: ", "build")
}

// SIGTERM during a run still exits 143, through the command's context.
func TestStepsExitWithSignalCodeUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTYEnv(t, t.TempDir(), stepsEnv, "smoke-steps", "--no-animations", "--wait")
	s.waitFor("waiting for cancellation")
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	s.waitExit(143)
	requireInOrder(t, s.text(), "✗ Build images", "│ cleaning up after cancellation", "pic-sure: ")
}

// CI selects plain output even on a terminal (spec §10.3).
func TestStepsArePlainInCIUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTYEnv(t, t.TempDir(), []string{"CI=true"}, "smoke-steps", "--no-animations")
	s.waitExit(0)
	requireInOrder(t, s.text(), "[SKIP]", "[ OK ] Fetch sources", "[ OK ] Finish", "smoke-steps finished")
}
