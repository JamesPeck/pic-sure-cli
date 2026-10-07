package progress

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
)

// syncBuffer is a bytes.Buffer safe for the program's writer goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestRenderer(out *syncBuffer) *Renderer {
	r := NewRenderer(RendererOptions{
		Input:   strings.NewReader(""),
		Output:  out,
		NoColor: true,
		LogPath: func() string { return "/stack/run.log" },
	})
	r.testOpts = []tea.ProgramOption{tea.WithWindowSize(80, 24)}
	return r
}

func TestRendererDrawsAndEndsOnResult(t *testing.T) {
	var out syncBuffer
	r := newTestRenderer(&out)
	r.Emit(events.StepStarted{ID: "a", Title: "Step A"})
	if _, err := r.Write([]byte("a log record\npartial")); err != nil {
		t.Fatal(err)
	}
	r.Emit(events.Log{ID: "a", Line: "boom"})
	r.Emit(events.StepDone{ID: "a", Status: events.StepFailed})
	r.Emit(events.Result{Error: &events.ErrorInfo{ExitCode: 1, Message: "x"}})

	// The program has exited: later output goes straight to Output.
	r.Emit(events.StepStarted{ID: "late", Title: "Late"})
	if _, err := r.Write([]byte(" line\n")); err != nil {
		t.Fatal(err)
	}
	r.Close()
	got := ansi.Strip(out.String())
	for _, want := range []string{"a log record", "Step A", "│ boom", "Log file: /stack/run.log", "partial line\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%q", want, got)
		}
	}
	if strings.Contains(got, "Late") {
		t.Errorf("an event after the Result was drawn:\n%q", got)
	}
}

func TestRendererWithoutEventsNeverStarts(t *testing.T) {
	var out syncBuffer
	r := newTestRenderer(&out)
	if _, err := r.Write([]byte("log\n")); err != nil {
		t.Fatal(err)
	}
	r.Emit(events.Result{OK: true})
	r.Close()
	if got := out.String(); got != "log\n" {
		t.Errorf("output = %q, want only the log line", got)
	}
}

// Close hands the terminal back; a later event starts a new program.
func TestRendererRestartsAfterClose(t *testing.T) {
	var out syncBuffer
	r := newTestRenderer(&out)
	r.Emit(events.StepStarted{ID: "a", Title: "Step A"})
	r.Emit(events.StepDone{ID: "a", Status: events.StepOK})
	r.Close()
	if _, err := out.Write([]byte("[editor]\n")); err != nil {
		t.Fatal(err)
	}
	r.Emit(events.StepStarted{ID: "b", Title: "Step B"})
	r.Emit(events.StepDone{ID: "b", Status: events.StepOK})
	r.Emit(events.Result{OK: true})
	requireOrder(t, ansi.Strip(out.String()), "Step A", "[editor]", "Step B")
}

func requireOrder(t *testing.T, text string, want ...string) {
	t.Helper()
	rest := text
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("missing %q (in order %q):\n%q", w, want, text)
		}
		rest = rest[i+len(w):]
	}
}
