package smoke

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

const (
	keyEnter = "\r"
	keyDown  = "\x1b[B"
	keyLeft  = "\x1b[D"
	keyEsc   = "\x1b"
)

// screenWatch renders a PTY session's output with the small terminal
// emulator, so tests can wait for text on the screen as drawn.
type screenWatch struct {
	t    *testing.T
	s    *ptySession
	scr  *screen
	read int
	rows uint16
}

func watchScreen(t *testing.T, s *ptySession) *screenWatch {
	return &screenWatch{t: t, s: s, scr: newScreen(40, 120), rows: 40}
}

func (w *screenWatch) frame() string {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	out := w.s.output.Bytes()
	w.scr.write(out[w.read:])
	w.read = len(out)
	return w.scr.text()
}

// wait waits up to timeout for every want string on the screen, and
// returns the screen.
func (w *screenWatch) wait(timeout time.Duration, want ...string) string {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		text := w.frame()
		ok := true
		for _, s := range want {
			ok = ok && strings.Contains(text, s)
		}
		if ok {
			return text
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("screen lacks %q after %s:\n%s", want, timeout, text)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// repaint makes the TUI redraw the whole screen, which the emulator
// renders more faithfully than a run of incremental updates.
func (w *screenWatch) repaint() {
	w.rows = 79 - w.rows // 40 and 39 alternately
	if err := pty.Setsize(w.s.master, &pty.Winsize{Rows: w.rows, Cols: 120}); err != nil {
		w.t.Fatal(err)
	}
}

// fakeDockerPath returns a PATH whose docker answers compose's ps, logs and
// restart for two services and the ownership check's listings with
// nothing, and fails everything else.
func fakeDockerPath(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
case "$*" in
"ps --all --no-trunc "*|"volume ls "*|"network ls "*) ;;
*" ps "*)
	echo '{"Service":"hpds","Name":"demo-hpds-1","State":"running","Health":"healthy","Status":"Up"}'
	echo '{"Service":"psama","Name":"demo-psama-1","State":"running","Health":"starting","Status":"Up"}' ;;
*" logs "*)
	for s in "$@"; do svc=$s; done
	echo "$svc-1  | $svc started"
	echo "$svc-1  | $svc is ready"
	exec sleep 60 ;;
*" restart "*) echo " Container demo-hpds-1  Restarting" >&2 ;;
*) echo "not faked: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

// renderedStack is finishedStack with a rendered compose file and secrets,
// so the compose verbs run.
func renderedStack(t *testing.T) string {
	t.Helper()
	dir := finishedStack(t)
	for name, data := range map[string]string{
		"pic-sure.yaml":                 "schema: 1\nname: demo\nauth: {mode: open, admin_email: admin@example.com}\n",
		".pic-sure/secrets.yaml":        "db_root_password: rootpw-for-the-smoke-test\n",
		".pic-sure/render/compose.yaml": "services: {}\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The dashboard lists compose's services, follows the selected one's logs,
// and runs an action on the run screen, then comes back.
func TestDashboardActionsUnderPTY(t *testing.T) {
	skipUnlessPTYAllowed(t)
	dir := renderedStack(t)
	s := startPTYEnv(t, dir, []string{"NO_COLOR=1", "PATH=" + fakeDockerPath(t)}, "--no-animations")
	w := watchScreen(t, s)
	w.wait(15*time.Second, "Dashboard")
	s.send(keyEnter)
	w.wait(15*time.Second, "PIC-SURE — demo", "running healthy", "psama", "Logs — hpds", "hpds is ready")

	s.send(keyDown)
	w.wait(10*time.Second, "Logs — psama", "psama is ready")

	s.send("l")
	w.wait(5*time.Second, "Load your data", "Demo dataset")
	s.send(keyEsc)
	w.wait(5*time.Second, "Logs — psama")

	s.send("r")
	w.wait(5*time.Second, "Restart psama?")
	s.send(keyLeft)
	s.send(keyEnter)
	w.wait(15*time.Second, "✓ Restarted psama", "enter to go back")
	s.send(keyEnter)
	w.repaint()
	w.wait(10*time.Second, "Services", "Logs — psama")

	s.send("X")
	w.wait(5*time.Second, "Type the stack's name, demo")
	s.send(keyEsc)
	w.repaint()
	w.wait(5*time.Second, "Logs — psama")

	s.send("q")
	s.waitExit0()
}

// TestDashboardOnARealStackUnderPTY drives the dashboard on a running stack
// (PICSURE_TUI_DASH_DIR) and writes the screens it saw to
// PICSURE_TUI_DASH_CAPTURE. It restarts the stack's second service, and runs
// migrate and update. With PICSURE_TUI_DASH_DESTROY set to the stack's name,
// it then destroys the stack.
func TestDashboardOnARealStackUnderPTY(t *testing.T) {
	dir := os.Getenv("PICSURE_TUI_DASH_DIR")
	if dir == "" {
		t.Skip("set PICSURE_TUI_DASH_DIR to a running stack")
	}
	s := startPTYEnv(t, dir, []string{"NO_COLOR=1"}, "--no-animations")
	w := watchScreen(t, s)
	var captured []string
	capture := func(title string, timeout time.Duration, want ...string) {
		t.Helper()
		w.wait(timeout, want...)
		// Redraw the whole screen once it settles: the emulator renders
		// that more faithfully than the incremental updates.
		w.repaint()
		time.Sleep(time.Second)
		captured = append(captured, "=== "+title+"\n"+w.wait(timeout, want...))
	}
	defer func() {
		if path := os.Getenv("PICSURE_TUI_DASH_CAPTURE"); path != "" {
			_ = os.WriteFile(path, []byte(strings.Join(captured, "\n\n")+"\n"), 0o644)
		}
	}()

	w.wait(15*time.Second, "Dashboard")
	s.send(keyEnter)
	capture("dashboard", 60*time.Second, "running", "release:", "Logs — ")
	s.send("h")
	capture("deep check", 3*time.Minute, "Health, checked")
	s.send(keyDown)
	capture("second service's logs", 30*time.Second, "Logs — ")
	s.send("r")
	capture("restart confirmation", 10*time.Second, "Restart ", "?")
	s.send(keyLeft)
	s.send(keyEnter)
	capture("restart finished", 3*time.Minute, "✓ Restarted ", "enter to go back")
	s.send(keyEnter)
	capture("back on the dashboard", 30*time.Second, "Health: h checks")
	s.send("R")
	capture("reset confirmation", 10*time.Second, "Type the stack's name")
	s.send(keyEsc)
	time.Sleep(500 * time.Millisecond) // esc and the next key together read as alt+key
	s.send("m")
	w.wait(10*time.Second, "Run the database migrations?")
	s.send("y")
	capture("migrate finished", 5*time.Minute, "✓ Migrations applied", "enter to go back")
	s.send(keyEnter)
	w.wait(30*time.Second, "Health: h checks")
	s.send("u")
	w.wait(10*time.Second, "Update PIC-SURE?")
	s.send("y")
	capture("update finished", 20*time.Minute, "✓ Update finished", "enter to go back")
	s.send(keyEnter)
	w.wait(30*time.Second, "Health: h checks")
	if os.Getenv("PICSURE_TUI_DASH_DESTROY") == "" {
		s.send("q")
		s.waitExit0()
		return
	}
	s.send("X")
	w.wait(10*time.Second, "Type the stack's name")
	s.send(os.Getenv("PICSURE_TUI_DASH_DESTROY") + keyEnter)
	capture("destroy finished", 5*time.Minute, "✓ Stack destroyed", "enter to go back")
	s.send(keyEnter)
	capture("landing after destroy", 30*time.Second, "Set up")
	s.send("q")
	s.waitExit0()
}
