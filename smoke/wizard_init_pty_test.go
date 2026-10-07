package smoke

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestWizardCreatesAStackUnderPTY drives the setup wizard to a real stack:
// a heavy, opt-in test. PICSURE_TUI_INIT_DIR is the stack directory (its
// base name is the stack name), PICSURE_TUI_INIT_PORTS is "HTTP,HTTPS",
// and PICSURE_TUI_INIT_CAPTURE, if set, is a file the screens are written
// to. It needs Docker, and leaves the stack running for the caller to tear
// down.
func TestWizardCreatesAStackUnderPTY(t *testing.T) {
	dir, ports := os.Getenv("PICSURE_TUI_INIT_DIR"), os.Getenv("PICSURE_TUI_INIT_PORTS")
	if dir == "" || ports == "" {
		t.Skip("set PICSURE_TUI_INIT_DIR and PICSURE_TUI_INIT_PORTS to create a real stack")
	}
	httpPort, httpsPort, _ := strings.Cut(ports, ",")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var captured []string
	s := startPTYEnv(t, dir, []string{"NO_COLOR=1"}, "--no-animations")
	scr := newScreen(40, 120)
	read := 0
	frame := func() string {
		s.mu.Lock()
		out := s.output.Bytes()
		scr.write(out[read:])
		read = len(out)
		s.mu.Unlock()
		return scr.text()
	}
	waitScreen := func(timeout time.Duration, want ...string) string {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			text := frame()
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(text, w)
			}
			if ok {
				return text
			}
			if time.Now().After(deadline) {
				t.Fatalf("screen lacks %q after %s:\n%s", want, timeout, text)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	// repaint makes the TUI redraw the whole screen, which the emulator
	// renders more faithfully than a run of incremental updates.
	rows := uint16(40)
	repaint := func() {
		rows = 79 - rows // 40 and 39 alternately
		if err := pty.Setsize(s.master, &pty.Winsize{Rows: rows, Cols: 120}); err != nil {
			t.Fatal(err)
		}
	}
	capture := func(title string, timeout time.Duration, want ...string) {
		t.Helper()
		time.Sleep(300 * time.Millisecond) // let the frame settle
		captured = append(captured, "=== "+title+"\n"+waitScreen(timeout, want...))
	}
	defer func() {
		if path := os.Getenv("PICSURE_TUI_INIT_CAPTURE"); path != "" {
			_ = os.WriteFile(path, []byte(strings.Join(captured, "\n\n")+"\n"), 0o644)
		}
	}()
	const (
		enter = "\r"
		down  = "\x1b[B"
		left  = "\x1b[D"
		clear = "\x15" // ctrl+u: delete to the start of the input
	)
	step := func(keys ...string) {
		for _, k := range keys {
			s.send(k)
			time.Sleep(150 * time.Millisecond)
		}
	}

	capture("landing", 15*time.Second, "Set up PIC-SURE")
	step(enter)
	capture("wizard: stack", 15*time.Second, "Stack name", "esc cancel")
	step(clear, filepath.Base(dir), enter, clear, "james_mono", enter, enter)
	capture("wizard: access", 15*time.Second, "Auth mode")
	step(down, enter, "admin@example.com", enter)
	capture("wizard: ports", 15*time.Second, "HTTP port")
	step(clear, httpPort, enter, clear, httpsPort, enter)
	step(enter) // database: local
	step(clear, "-XX:+UseParallelGC -XX:SurvivorRatio=250 -Xms1g -Xmx2g", enter)
	step(enter) // no proxy
	capture("wizard: confirm", 15*time.Second, "Create the stack")
	step(left, enter)
	capture("run: first steps", 2*time.Minute, "Setting up PIC-SURE", "Check the host")
	capture("run: building", 30*time.Minute, "Write the config and secrets")
	waitScreen(45*time.Minute, "Setup finished")
	repaint()
	capture("run: finished", 30*time.Second, "Setup finished")
	step(enter)
	waitScreen(30*time.Second, "Dashboard")
	repaint()
	capture("landing on the new stack", 30*time.Second, "Dashboard")
	step("q")
	s.waitExit0()
}
