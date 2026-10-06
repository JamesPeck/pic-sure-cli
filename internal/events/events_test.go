package events

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestEventJSON(t *testing.T) {
	pct := 42.5
	tests := []struct {
		event Event
		want  string
	}{
		{StepStarted{ID: "db", Title: "Start the database"}, `{"id":"db","title":"Start the database"}`},
		{Progress{ID: "build", Text: "3/11 images"}, `{"id":"build","text":"3/11 images"}`},
		{Progress{ID: "build", Text: "3/11 images", Pct: &pct}, `{"id":"build","text":"3/11 images","pct":42.5}`},
		{Log{ID: "build", Stream: StreamStderr, Line: "BUILD OK"}, `{"id":"build","stream":"stderr","line":"BUILD OK"}`},
		{Warning{Text: "build-spec has no PSCLI entry"}, `{"text":"build-spec has no PSCLI entry"}`},
		{StepDone{ID: "db", Status: StepSkipped}, `{"id":"db","status":"skipped"}`},
		{Result{OK: true}, `{"ok":true}`},
		{
			Result{Error: &ErrorInfo{ExitCode: 3, Message: "docker daemon unreachable", Step: "preconditions"}},
			`{"ok":false,"error":{"exit_code":3,"message":"docker daemon unreachable","step":"preconditions"}}`,
		},
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.event)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("%s: got %s, want %s", tt.event.Type(), got, tt.want)
		}
	}
}

func TestEventTypesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range []Event{StepStarted{}, Progress{}, Log{}, Warning{}, StepDone{}, Result{}} {
		if seen[e.Type()] {
			t.Errorf("duplicate event type %q", e.Type())
		}
		seen[e.Type()] = true
	}
}

func TestRecorderConcurrentEmit(t *testing.T) {
	var r Recorder
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Emit(Warning{Text: "w"})
		}()
	}
	wg.Wait()
	if got := len(r.Events()); got != 50 {
		t.Errorf("recorded %d events, want 50", got)
	}
	types := r.Types()
	if len(types) != 50 || types[0] != "warning" {
		t.Errorf("Types() = %v", types)
	}
}

func TestLogWriter(t *testing.T) {
	var r Recorder
	w := NewLogWriter(&r, "build", StreamStdout)
	for _, chunk := range []string{"first li", "ne\r\nsecond\n", "\nthird", " part"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range r.Events() {
		l := e.(Log)
		if l.ID != "build" || l.Stream != StreamStdout {
			t.Errorf("event %+v has the wrong ID or stream", l)
		}
		lines = append(lines, l.Line)
	}
	want := []string{"first line", "second", "", "third part"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}

func TestLogWriterSplitsOverlongLines(t *testing.T) {
	var r Recorder
	w := NewLogWriter(&r, "build", StreamStderr)
	_, _ = w.Write([]byte(strings.Repeat("x", maxLineLen+10)))
	_ = w.Close()
	events := r.Events()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if n := len(events[0].(Log).Line); n != maxLineLen {
		t.Errorf("first piece is %d bytes, want %d", n, maxLineLen)
	}
	if n := len(events[1].(Log).Line); n != 10 {
		t.Errorf("second piece is %d bytes, want 10", n)
	}
}
