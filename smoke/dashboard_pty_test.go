// End-to-end PTY tests: the built binary must start the TUI on a terminal,
// render the landing, the setup wizard and the dashboard, and quit cleanly on
// 'q' without corrupting the terminal (a clean exit implies Bubble Tea's
// teardown ran).
package smoke

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/JamesPeck/pic-sure-cli/internal/styles/stylestest"
)

// bin is the pic-sure binary TestMain builds for every test.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pic-sure-smoke")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "pic-sure")
	out, err := exec.Command("go", "build", "-o", bin, "../cmd/pic-sure").CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		panic(fmt.Sprintf("go build: %v\n%s", err, out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func skipUnlessPTYAllowed(t *testing.T) {
	t.Helper()
	if os.Getenv("CI") != "" && os.Getenv("PICSURE_PTY_TEST") == "" {
		// PTY behavior on exotic CI runners can be flaky; opt in there.
		t.Skip("set PICSURE_PTY_TEST=1 to run the PTY test in CI")
	}
}

// ptySession runs the binary on a 120x40 pseudo-terminal and collects its
// output.
type ptySession struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	mu     sync.Mutex
	output bytes.Buffer
}

// startPTY runs the binary in dir with the test's environment.
func startPTY(t *testing.T, dir string, args ...string) *ptySession {
	t.Helper()
	return startPTYEnv(t, dir, nil, args...)
}

// startPTYEnv is startPTY with extra NAME=value entries in the environment.
func startPTYEnv(t *testing.T, dir string, env []string, args ...string) *ptySession {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	s := &ptySession{t: t, cmd: cmd, master: master}
	t.Cleanup(func() { _ = master.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.output.Write(buf[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

// waitFor waits up to 15s for every want string to appear in the output.
func (s *ptySession) waitFor(want ...string) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		rendered := s.output.String()
		s.mu.Unlock()
		found := true
		for _, w := range want {
			found = found && strings.Contains(rendered, w)
		}
		if found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t.Fatalf("did not render %q within 15s; output:\n%s", want, s.output.String())
}

func (s *ptySession) send(keys string) {
	s.t.Helper()
	if _, err := s.master.Write([]byte(keys)); err != nil {
		s.t.Fatal(err)
	}
}

// waitExit0 requires the process to exit 0 within 10s.
func (s *ptySession) waitExit0() {
	s.t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			s.t.Fatalf("exited non-zero: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		s.t.Fatal("did not quit within 10s")
	}
}

func TestLandingStartsAndQuitsUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTY(t, t.TempDir(), "--no-animations")
	s.waitFor("Set up PIC-SURE")
	s.send("q")
	s.waitExit0()
}

func TestDashboardStartsAndQuitsUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	// The landing still detects a configured stack by the v1 .env file;
	// with one present, its first entry opens the dashboard.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := startPTY(t, dir, "--no-animations")
	s.waitFor("Dashboard")
	s.send("\r")
	s.waitFor("PIC-SURE", "Services")
	s.send("q")
	s.waitExit0()
}

func TestWizardOpensAndClosesUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	// The setup wizard still seeds itself from the v1 .env.example.
	dir := t.TempDir()
	example := "DB_MODE=local\nAUTH_MODE=open\n"
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte(example), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startPTY(t, dir, "--no-animations")
	s.waitFor("Set up PIC-SURE")
	s.send("\r")
	s.waitFor("Identity provider", "esc cancel")
	s.send("\x1b") // a pristine form closes without asking
	s.waitFor("setup cancelled")
	s.send("q")
	s.waitExit0()
}

// The TUI asks the terminal for its background color and picks the palette
// to match: on a light background the dashboard header is the exact logo blue
// (#224D96) rather than the lifted dark-background variant.
func TestTUIFollowsTerminalBackground(t *testing.T) {
	skipUnlessPTYAllowed(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := startPTYEnv(t, dir, []string{"TERM=xterm-256color", "COLORTERM=truecolor", "NO_COLOR="}, "--no-animations")
	s.waitFor("\x1b]11;?") // the background color query (OSC 11)
	s.send("\x1b]11;rgb:ffff/ffff/ffff\x07")
	s.waitFor("Dashboard")
	s.send("\r")
	s.waitFor("Services", "38;2;34;77;150")
	s.send("q")
	s.waitExit0()
}

// Any non-empty NO_COLOR turns colors off (text decoration stays), even on a
// truecolor terminal and even for values that don't parse as true.
func TestTUIHonoursNoColor(t *testing.T) {
	skipUnlessPTYAllowed(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := startPTYEnv(t, dir, []string{"TERM=xterm-256color", "COLORTERM=truecolor", "NO_COLOR=yes"}, "--no-animations")
	s.waitFor("Dashboard")
	s.send("\r")
	s.waitFor("Services", "not implemented")
	s.send("q")
	s.waitExit0()
	s.mu.Lock()
	defer s.mu.Unlock()
	if stylestest.HasColor(s.output.String()) {
		t.Errorf("NO_COLOR=yes: the TUI still set colors:\n%q", s.output.String())
	}
}

// SIGTERM ends the TUI through the CLI's context: the terminal is restored
// and the process exits 143 itself rather than dying by the signal.
func TestTUIExitsWithSignalCode(t *testing.T) {
	skipUnlessPTYAllowed(t)
	s := startPTY(t, t.TempDir(), "--no-animations")
	s.waitFor("Set up PIC-SURE")
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
			t.Fatalf("exit = %v, want exit status 143", err)
		}
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		t.Fatal("did not exit within 10s of SIGTERM")
	}
}

func TestBareInvocationNonTTYPrintsHelp(t *testing.T) {
	cmd := exec.Command(bin)
	cmd.Stdin = nil // not a terminal
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bare non-TTY invocation should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Errorf("expected help output, got:\n%s", out)
	}
}
