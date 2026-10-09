package progress

import (
	"fmt"
	"math"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
)

// drive feeds msgs to m in order, acknowledging each print the way the
// program would, and returns the model and the last command.
func drive(t *testing.T, m Model, msgs ...tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, msg := range msgs {
		var next tea.Model
		next, cmd = m.Update(msg)
		m = next.(Model)
		for m.printing {
			next, cmd = m.Update(printedMsg{})
			m = next.(Model)
		}
	}
	return m, cmd
}

func evs(es ...events.Event) []tea.Msg {
	msgs := make([]tea.Msg, len(es))
	for i, e := range es {
		msgs[i] = EventMsg{Event: e}
	}
	return msgs
}

func view(m Model) string { return ansi.Strip(m.View().Content) }

// recording returns a model that records what it prints.
func recording(opts Options) (Model, *[]string) {
	var log []string
	m := New(opts)
	m.printHook = func(s string) { log = append(log, s) }
	return m, &log
}

func joined(log *[]string) string { return ansi.Strip(strings.Join(*log, "\n")) }

func pct(v float64) *float64 { return &v }

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// An init-like run: steps that apply, skip by Check and skip by --skip-step,
// with progress, log lines and warnings.
var initRun = []events.Event{
	events.StepStarted{ID: "release", Title: "Fetch release-control"},
	events.StepDone{ID: "release", Status: events.StepOK},
	events.StepStarted{ID: "tls", Title: "Install TLS certificates"},
	events.Warning{ID: "tls", Text: "skipped by --skip-step"},
	events.StepDone{ID: "tls", Status: events.StepSkipped},
	events.StepStarted{ID: "build", Title: "Build images"},
	events.Progress{ID: "build", Text: "pic-sure-hpds", Pct: pct(42)},
}

func TestScrollbackPrintsFinishedStepsAndShowsTheRunningOne(t *testing.T) {
	es := append([]events.Event{}, initRun...)
	for i := 1; i <= 12; i++ {
		es = append(es, events.Log{ID: "build", Stream: "stdout", Line: fmt.Sprintf("maven line %d", i)})
	}
	m, log := recording(Options{Scrollback: true})
	m, _ = drive(t, m, evs(es...)...)

	want := "✓ Fetch release-control\n- Install TLS certificates (skipped)\n  ! skipped by --skip-step"
	if got := joined(log); got != want {
		t.Errorf("printed:\n%s\nwant:\n%s", got, want)
	}
	v := view(m)
	if !strings.HasPrefix(v, "• Build images · pic-sure-hpds (42%)\n") {
		t.Errorf("live area doesn't start with the running step:\n%s", v)
	}
	if strings.Contains(v, "maven line 4\n") || !strings.Contains(v, "│ maven line 5\n") || !strings.HasSuffix(v, "│ maven line 12") {
		t.Errorf("live area should tail the last %d log lines:\n%s", LiveTail, v)
	}
	if strings.Contains(v, "Fetch release-control") {
		t.Errorf("a printed step is still in the live area:\n%s", v)
	}

	m, cmd := drive(t, m, append(evs(events.StepDone{ID: "build", Status: events.StepOK}), DoneMsg{OK: true, LogPath: "/x.log"})...)
	if !isQuit(cmd) {
		t.Error("a finished run should quit")
	}
	if v := view(m); v != "" {
		t.Errorf("a successful run leaves nothing in the live area, got:\n%s", v)
	}
	if got := joined(log); !strings.HasSuffix(got, "\n✓ Build images") || strings.Contains(got, "maven") {
		t.Errorf("a step that succeeded is printed without its log:\n%s", got)
	}
}

func TestFailureKeepsTheLastLogLinesAndTheLogFile(t *testing.T) {
	es := append([]events.Event{}, initRun...)
	for i := 1; i <= 30; i++ {
		es = append(es, events.Log{ID: "build", Line: fmt.Sprintf("line %d", i)})
	}
	es = append(es, events.StepDone{ID: "build", Status: events.StepFailed})
	m, log := recording(Options{Scrollback: true})
	m, cmd := drive(t, m, append(evs(es...), DoneMsg{LogPath: "/stack/.pic-sure/logs/cli.log"})...)
	if !isQuit(cmd) {
		t.Error("a failed run should quit")
	}
	got := joined(log)
	if !strings.Contains(got, "✗ Build images\n") || strings.Contains(got, "line 10\n") ||
		!strings.Contains(got, "│ line 11\n") || !strings.HasSuffix(got, "│ line 30") {
		t.Errorf("a failed step keeps its last %d log lines:\n%s", FailTail, got)
	}
	if v := view(m); v != "Log file: /stack/.pic-sure/logs/cli.log" {
		t.Errorf("final view = %q, want the log file", v)
	}
}

// When the run ends, everything not yet printed is printed after the print
// in flight, so a final frame taller than the terminal can't clip it.
func TestDonePrintsTheRestAfterThePrintInFlight(t *testing.T) {
	m, log := recording(Options{Scrollback: true, NoColor: true})
	next, _ := m.Update(EventMsg{Event: events.StepStarted{ID: "a", Title: "A"}})
	next, _ = next.Update(EventMsg{Event: events.StepDone{ID: "a", Status: events.StepOK}})
	m = next.(Model)
	if !m.printing {
		t.Fatal("a finished step should start printing")
	}
	next, _ = m.Update(printMsg{text: "a log record"})
	next, _ = next.Update(EventMsg{Event: events.StepStarted{ID: "b", Title: "B"}})
	next, _ = next.Update(EventMsg{Event: events.StepDone{ID: "b", Status: events.StepFailed}})
	next, _ = next.Update(EventMsg{Event: events.StepStarted{ID: "c", Title: "C"}})
	next, cmd := next.Update(DoneMsg{LogPath: "/l"})
	if cmd != nil {
		t.Error("printed or quit before the print in flight finished")
	}
	next, cmd = next.Update(printedMsg{})
	if cmd == nil || isQuit(cmd) {
		t.Error("should print the rest once the print in flight finished")
	}
	next, cmd = next.Update(printedMsg{})
	if !isQuit(cmd) {
		t.Error("should quit once everything is printed")
	}
	m = next.(Model)
	if got := strings.Join(*log, "\n"); got != "✓ A\na log record\n✗ B\n• C" {
		t.Errorf("printed %q (NoColor must print no escapes)", got)
	}
	if v := view(m); v != "Log file: /l" {
		t.Errorf("final view = %q", v)
	}
}

func TestEmbeddedViewShowsEveryStepAndDoesNotQuit(t *testing.T) {
	m, log := recording(Options{})
	m, cmd := drive(t, m, append(evs(initRun...), DoneMsg{OK: true})...)
	if cmd != nil {
		t.Error("an embedded model must not quit the host program")
	}
	if len(*log) != 0 {
		t.Errorf("an embedded model printed %q", *log)
	}
	v := view(m)
	for _, want := range []string{"✓ Fetch release-control", "- Install TLS certificates (skipped)", "• Build images · pic-sure-hpds (42%)"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if !m.Done() {
		t.Error("Done() = false after DoneMsg")
	}
}

func TestCtrlCAsksThenInterrupts(t *testing.T) {
	interrupts := 0
	m, _ := drive(t, New(Options{Scrollback: true, ForceQuit: true, Interrupt: func() { interrupts++ }}), evs(initRun...)...)
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}

	m, cmd := drive(t, m, ctrlC)
	if interrupts != 0 || !strings.Contains(view(m), "Press Ctrl-C again to cancel.") || cmd == nil {
		t.Fatalf("first Ctrl-C should ask, with a timeout; interrupts=%d view:\n%s", interrupts, view(m))
	}
	m, _ = drive(t, m, tea.KeyPressMsg{Code: 'x'})
	if strings.Contains(view(m), "Press Ctrl-C") {
		t.Error("another key should withdraw the prompt")
	}

	m, _ = drive(t, m, ctrlC)
	m, _ = drive(t, m, confirmExpiredMsg{seq: m.confirmSeq})
	if strings.Contains(view(m), "Press Ctrl-C") {
		t.Error("the prompt should expire")
	}

	m, _ = drive(t, m, ctrlC)
	stale := confirmExpiredMsg{seq: m.confirmSeq - 1}
	m, _ = drive(t, m, stale, ctrlC)
	if interrupts != 1 || !strings.Contains(view(m), "Ctrl-C again to quit now") || !m.Cancelling() {
		t.Fatalf("second Ctrl-C should interrupt once; interrupts=%d view:\n%s", interrupts, view(m))
	}
	if m.Running() != "Build images" {
		t.Errorf("Running() = %q, want the step that was stopping", m.Running())
	}

	// The operation stops: its step fails, and the run ends.
	stopped, _ := drive(t, m, EventMsg{Event: events.StepDone{ID: "build", Status: events.StepFailed}}, DoneMsg{LogPath: "/l"})
	if v := view(stopped); v != "Log file: /l" {
		t.Errorf("final view = %q", v)
	}

	// Or it doesn't, and another Ctrl-C forces a quit, as a second SIGINT
	// would.
	m, cmd = drive(t, m, tea.KeyPressMsg{Code: 'x'})
	if m.Forced || cmd != nil {
		t.Error("only Ctrl-C forces a quit")
	}
	m, cmd = drive(t, m, ctrlC)
	if !m.Forced || !isQuit(cmd) || interrupts != 1 {
		t.Errorf("Ctrl-C while cancelling should force a quit: forced=%v interrupts=%d", m.Forced, interrupts)
	}
}

// Without ForceQuit, Ctrl-C while the operation stops does nothing.
func TestCtrlCWithoutForceQuitOnlyCancels(t *testing.T) {
	m, _ := drive(t, New(Options{Interrupt: func() {}}), evs(initRun...)...)
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	m, _ = drive(t, m, ctrlC, ctrlC)
	m, cmd := drive(t, m, ctrlC)
	if m.Forced || cmd != nil || strings.Contains(view(m), "quit now") {
		t.Errorf("forced=%v cmd=%v view:\n%s", m.Forced, cmd != nil, view(m))
	}
}

// A multi-line warning keeps its lines, the rest indented under the first,
// as the plain renderer prints it.
func TestMultiLineWarning(t *testing.T) {
	m, _ := drive(t, New(Options{}), evs(
		events.StepStarted{ID: "s", Title: "S"},
		events.Warning{ID: "s", Text: "hpds restart failed: exit 1\nstderr: no such container\n"},
		events.Warning{Text: "orphan: first\r\nsecond"},
	)...)
	v := view(m)
	for _, want := range []string{
		"  ! hpds restart failed: exit 1\n    stderr: no such container\n",
		"! orphan: first\n  second",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
}

func TestAnimations(t *testing.T) {
	if New(Options{}).Init() != nil {
		t.Error("without animations Init should not start the spinner")
	}
	if New(Options{Animations: true}).Init() == nil {
		t.Error("with animations Init should start the spinner")
	}
	m, _ := drive(t, New(Options{Animations: true}), evs(events.StepStarted{ID: "a", Title: "A"})...)
	if strings.HasPrefix(view(m), "• ") {
		t.Errorf("an animated step shows the spinner, got %q", view(m))
	}
}

func TestOrphanEventsAndWidth(t *testing.T) {
	m, _ := drive(t, New(Options{}), evs(
		events.Warning{Text: "no step owns this"},
		events.Progress{ID: "wait", Text: "waiting for the reactor lock", Pct: pct(math.NaN())},
		events.Log{ID: "wait", Line: "\x1b[32mgreen\x1b[0m\tthen\r50%\r100%"},
	)...)
	v := view(m)
	for _, want := range []string{"! no step owns this", "waiting for the reactor lock\n", "│ 100%"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}

	m, _ = drive(t, m, EventMsg{Event: events.StepStarted{ID: "s", Title: "S"}},
		EventMsg{Event: events.Log{ID: "other", Line: "routed to the running step"}},
		tea.WindowSizeMsg{Width: 10, Height: 5})
	if !strings.Contains(view(m), "│ route…") {
		t.Errorf("a log line for an unknown step goes under the running one, truncated to the width:\n%s", view(m))
	}
}

func TestCleanLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                             "plain",
		"a\u009b31mred":                     "a31mred",
		"a\tb":                              "a b",
		"10%\r20%\r":                        "20%",
		"\x1b[1mbold\x1b[0m\x07":            "bold",
		"\x1b]8;;http://x\x07l\x1b]8;;\x07": "l",
	} {
		if got := cleanLine(in); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}
