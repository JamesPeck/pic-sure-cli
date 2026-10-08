package smoke

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestLandingOnARealStackUnderPTY drives the landing's actions on a running
// stack and ends by destroying it, so it runs only when
// PICSURE_TUI_LANDING_DIR names a throwaway stack and
// PICSURE_TUI_LANDING_NAME its name. PICSURE_TUI_LANDING_CAPTURE writes the
// screens it saw to a file.
func TestLandingOnARealStackUnderPTY(t *testing.T) {
	dir, name := os.Getenv("PICSURE_TUI_LANDING_DIR"), os.Getenv("PICSURE_TUI_LANDING_NAME")
	if dir == "" || name == "" {
		t.Skip("set PICSURE_TUI_LANDING_DIR and PICSURE_TUI_LANDING_NAME to a throwaway running stack")
	}
	s := startPTYEnv(t, dir, []string{"NO_COLOR=1"}, "--no-animations")
	w := watchScreen(t, s)
	var captured []string
	capture := func(title string, timeout time.Duration, want ...string) {
		t.Helper()
		w.wait(timeout, want...)
		w.repaint()
		time.Sleep(time.Second)
		captured = append(captured, "=== "+title+"\n"+w.wait(timeout, want...))
	}
	defer func() {
		if path := os.Getenv("PICSURE_TUI_LANDING_CAPTURE"); path != "" {
			_ = os.WriteFile(path, []byte(strings.Join(captured, "\n\n")+"\n"), 0o644)
		}
	}()
	esc := func() {
		s.send(keyEsc)
		time.Sleep(500 * time.Millisecond) // esc and the next key together read as alt+key
	}
	down := func(n int) {
		for range n {
			s.send(keyDown)
		}
	}
	// back leaves a finished run screen for the landing's menu.
	back := func() {
		s.send(keyEnter)
		w.wait(30*time.Second, "Back")
	}

	w.wait(15*time.Second, "Developer options")
	down(1)
	s.send(keyEnter)
	w.wait(10*time.Second, "Update PIC-SURE?")
	s.send("y")
	capture("update", 20*time.Minute, "✓ Update finished", "enter to go back")
	s.send(keyEnter)
	w.wait(30*time.Second, "Developer options")
	down(2)
	s.send(keyEnter)
	w.wait(10*time.Second, "Preview update")

	s.send(keyEnter) // Preflight check
	capture("preflight check", 3*time.Minute, "enter to go back")
	back()
	down(1)
	s.send(keyEnter) // Preview update
	w.wait(10*time.Second, "Preview the update?")
	s.send("y")
	capture("preview update", 5*time.Minute, "enter to go back")
	back()
	down(2)
	s.send(keyEnter) // Run migrations
	w.wait(10*time.Second, "Run the database migrations?")
	s.send("y")
	capture("migrate", 5*time.Minute, "✓ Migrations applied", "enter to go back")
	back()
	down(2)
	s.send(keyEnter) // Rebuild dictionary
	w.wait(10*time.Second, "Rebuild the dictionary")
	s.send(keyEnter)
	capture("dictionary hydrate", 10*time.Minute, "enter to go back")
	back()
	down(1)
	s.send(keyEnter) // Dev mode on
	w.wait(10*time.Second, "psama")
	w.repaint() // the emulator can drop the dialog's title from the incremental update
	capture("dev mode picker", 10*time.Second, "Dev mode on", "psama")
	esc()
	down(2)
	s.send(keyEnter) // Reset
	w.wait(10*time.Second, "Type the stack's name")
	down(1)
	s.send(keyEnter)                   // Keep the database
	time.Sleep(500 * time.Millisecond) // huh moves the focus to the input in a command
	s.send(name + keyEnter)
	capture("reset keeping the database", 5*time.Minute, "✓ Stack reset", "enter to go back")
	back()
	down(1)
	s.send(keyEnter) // Destroy
	w.wait(10*time.Second, "Type the stack's name")
	s.send(name + keyEnter)
	capture("destroy", 5*time.Minute, "✓ Stack destroyed", "enter to go back")
	s.send(keyEnter)
	capture("landing after destroy", 30*time.Second, "Set up")
	down(1)
	s.send(keyEnter) // Preflight check, without a stack
	capture("preflight check without a stack", 3*time.Minute, "enter to go back")
	s.send(keyEnter)
	w.wait(30*time.Second, "Set up")
	s.send("q")
	s.waitExit0()
}
