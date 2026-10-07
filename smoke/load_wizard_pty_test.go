package smoke

import (
	"os"
	"strings"
	"testing"
	"time"
)

const (
	keyUp    = "\x1b[A"
	keyRight = "\x1b[C"
)

// TestLoadWizardOnARealStackUnderPTY loads data through the load wizard
// into a running stack: a heavy, opt-in test. PICSURE_TUI_LOAD_DIR is the
// stack directory and PICSURE_TUI_LOAD_FLOW the load to run:
//
//   - dir: from the landing, the directory "1-pheno-dir" in the stack
//     directory (the first directory there), with the auto dictionary;
//   - archive: from the dashboard, the second CSV of the only file in the
//     directory "2-archive", a tar.gz holding two;
//   - demo: from the dashboard, the NHANES demo data.
//
// PICSURE_TUI_LOAD_CAPTURE, if set, is a file the screens are written to.
func TestLoadWizardOnARealStackUnderPTY(t *testing.T) {
	dir, flow := os.Getenv("PICSURE_TUI_LOAD_DIR"), os.Getenv("PICSURE_TUI_LOAD_FLOW")
	if dir == "" || flow == "" {
		t.Skip("set PICSURE_TUI_LOAD_DIR and PICSURE_TUI_LOAD_FLOW to load into a running stack")
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
		if path := os.Getenv("PICSURE_TUI_LOAD_CAPTURE"); path != "" {
			_ = os.WriteFile(path, []byte(strings.Join(captured, "\n\n")+"\n"), 0o644)
		}
	}()
	// confirm answers the confirm summary's Load.
	confirm := func(title string, want ...string) {
		capture(title, 30*time.Second, want...)
		s.send(keyLeft)
		s.send(keyEnter)
	}

	w.wait(15*time.Second, "Dashboard", "Load your data")
	if flow == "dir" {
		s.send(keyDown + keyDown + keyEnter)
		capture("kind", 10*time.Second, "Choose the kind of data")
		s.send(keyDown + keyEnter)
		capture("directory browser", 10*time.Second, "Select the directory of phenotype CSVs", "1-pheno-dir")
		s.send(keyEnter)
		capture("heap", 30*time.Second, "JVM heap size (MB)", "8000")
		s.send(keyEnter)
		w.wait(10*time.Second, "Dictionary")
		s.send(keyEnter)
		confirm("confirm", "Load phenotype data", "1-pheno-dir")
		capture("load finished", 10*time.Minute, "✓ Phenotype data loaded", "enter to go back")
		s.send(keyEnter)
		w.wait(30*time.Second, "Load your data")
		s.send("q")
		s.waitExit0()
		return
	}

	s.send(keyEnter)
	w.wait(60*time.Second, "Logs — ")
	s.send("l")
	capture("kind, over the dashboard", 10*time.Second, "Choose the kind of data")
	switch flow {
	case "archive":
		s.send(keyEnter)
		w.wait(10*time.Second, "Select the phenotype CSV", "2-archive")
		s.send(keyDown + keyRight)
		capture("file browser", 10*time.Second, "two-csvs.tar.gz")
		s.send(keyEnter)
		capture("entry picker", 30*time.Second, "Choose the CSV to load", "b/second.csv")
		s.send(keyDown + keyEnter)
		w.wait(10*time.Second, "JVM heap size (MB)")
		s.send(keyEnter)
		w.wait(10*time.Second, "Dictionary")
		s.send(keyEnter)
		confirm("confirm", "Archive entry", "b/second.csv")
		capture("load finished", 10*time.Minute, "✓ Phenotype data loaded", "enter to go back")
	case "demo":
		s.send(keyDown + keyDown + keyEnter)
		capture("datasets", 10*time.Second, "Demo dataset", "NHANES")
		s.send(keyEnter)
		w.wait(10*time.Second, "JVM heap size (MB)")
		s.send(keyEnter)
		confirm("confirm", "Load demo data", "nhanes")
		capture("load finished", 15*time.Minute, "✓ Demo data loaded", "enter to go back")
	default:
		t.Fatalf("PICSURE_TUI_LOAD_FLOW %q: want dir, archive or demo", flow)
	}
	s.send(keyEnter)
	capture("back on the dashboard", 30*time.Second, "Logs — ")
	s.send("q")
	s.waitExit0()
}
