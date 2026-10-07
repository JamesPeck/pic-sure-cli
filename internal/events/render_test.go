package events

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// recorded is a converging operation's events as the step engine, its steps
// and the cli layer emit them: a step that runs, one skipped by its check,
// one with progress and a multi-line warning, and one that fails.
func recorded() []Event {
	pct, full := 27.4, 100.0
	return []Event{
		StepStarted{ID: "db", Title: "Start the database"},
		Log{ID: "db", Stream: StreamStdout, Line: "Container ws-demo-picsure-db-1  Started"},
		Log{ID: "db", Stream: StreamStderr, Line: ""},
		StepDone{ID: "db", Status: StepOK},
		StepStarted{ID: "seed", Title: "Seed the database"},
		StepDone{ID: "seed", Status: StepSkipped},
		StepStarted{ID: "build", Title: "Build images"},
		Progress{ID: "build", Text: "3/11 images", Pct: &pct},
		Progress{ID: "build", Text: "waiting for the reactor lock"},
		Warning{ID: "build", Text: "build-spec has no PSCLI entry\nassuming this CLI is current"},
		Progress{ID: "build", Text: "11/11 images", Pct: &full},
		StepDone{ID: "build", Status: StepOK},
		StepStarted{ID: "migrate", Title: "Run <flyway> migrations & seed"},
		Log{ID: "migrate", Stream: StreamStderr, Line: "ERROR: Validate failed: \"V3\" checksum mismatch"},
		StepDone{ID: "migrate", Status: StepFailed},
		Result{Error: &ErrorInfo{ExitCode: 1, Message: "step migrate failed", Step: "migrate"}},
	}
}

// ticking returns a clock that starts at 09:00:00 UTC and advances a second
// per call.
func ticking() func() time.Time {
	t := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

// golden compares got with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (go test -update rewrites it):\n got:\n%s\nwant:\n%s", path, got, want)
	}
}

func render(s Sink, events []Event) {
	for _, e := range events {
		s.Emit(e)
	}
}

func TestPlainGolden(t *testing.T) {
	var buf bytes.Buffer
	render(NewPlain(&buf, PlainOptions{Now: ticking()}), recorded())
	golden(t, "plain.golden", buf.Bytes())
}

func TestPlainColorGolden(t *testing.T) {
	var buf bytes.Buffer
	render(NewPlain(&buf, PlainOptions{Color: true, Now: ticking()}), recorded())
	golden(t, "plain_color.golden", buf.Bytes())

	var plain bytes.Buffer
	render(NewPlain(&plain, PlainOptions{Now: ticking()}), recorded())
	if stripped := sgr.ReplaceAllString(buf.String(), ""); stripped != plain.String() {
		t.Errorf("colour output without its escapes differs from plain output:\n%s", stripped)
	}
}

var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestPlainEdgeCases(t *testing.T) {
	nan, over, under := math.NaN(), 250.0, -3.0
	var buf bytes.Buffer
	render(NewPlain(&buf, PlainOptions{Now: ticking()}), []Event{
		StepDone{ID: "orphan", Status: StepOK}, // no StepStarted: the ID stands in for the title
		Progress{ID: "x", Text: "nan", Pct: &nan},
		Progress{ID: "x", Text: "over", Pct: &over},
		Progress{ID: "x", Text: "under", Pct: &under},
		Warning{Text: "trailing newline\n"},
		Result{OK: true, Data: map[string]int{"n": 1}},
	})
	want := strings.Join([]string{
		"09:00:01 [ OK ] orphan",
		"09:00:02 [ .. ] nan",
		"09:00:03 [100%] over",
		"09:00:04 [  0%] under",
		"09:00:05 [WARN] trailing newline",
		"",
	}, "\n")
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestNDJSONGolden(t *testing.T) {
	var buf bytes.Buffer
	n := NewNDJSON(&buf)
	render(n, recorded())
	golden(t, "ndjson.golden", buf.Bytes())

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != len(recorded()) {
		t.Fatalf("%d lines for %d events", len(lines), len(recorded()))
	}
	for i, line := range lines {
		want := recorded()[i].Type()
		if !strings.HasPrefix(line, `{"type":"`+want+`"`) {
			t.Errorf("line %d does not start with type %q: %s", i+1, want, line)
		}
	}
}

func TestNDJSONResultWithData(t *testing.T) {
	type report struct {
		URL      string   `json:"url"`
		Services []string `json:"services"`
	}
	var buf bytes.Buffer
	NewNDJSON(&buf).Emit(Result{OK: true, Data: report{URL: "https://localhost:8443/?a=1&b=2", Services: []string{"httpd"}}})
	want := `{"type":"result","ok":true,"data":{"url":"https://localhost:8443/?a=1&b=2","services":["httpd"]}}` + "\n"
	if buf.String() != want {
		t.Errorf("got  %s\nwant %s", buf.String(), want)
	}
}

func TestNDJSONUnencodableEvents(t *testing.T) {
	nan := math.NaN()
	var buf bytes.Buffer
	n := NewNDJSON(&buf)
	n.Emit(Progress{ID: "build", Text: "0/0 images", Pct: &nan})
	n.Emit(Result{OK: true, Data: map[string]any{"ch": make(chan int)}})
	want := `{"type":"progress","id":"build","text":"0/0 images"}` + "\n" +
		`{"type":"warning","text":"cannot encode a result event: json: unsupported type: chan int"}` + "\n"
	if buf.String() != want {
		t.Errorf("got:\n%swant:\n%s", buf.String(), want)
	}
}

func TestNDJSONConcurrentEmitKeepsLinesWhole(t *testing.T) {
	var buf bytes.Buffer
	n := NewNDJSON(&buf)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				n.Emit(Log{ID: "build", Stream: StreamStdout, Line: strings.Repeat(string(rune('a'+i)), 500)})
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 1000 {
		t.Fatalf("%d lines, want 1000", len(lines))
	}
	for _, line := range lines {
		var l Log
		if err := json.Unmarshal([]byte(line), &l); err != nil || len(l.Line) != 500 {
			t.Fatalf("mangled line %q: %v", line, err)
		}
	}
}

func TestWriteReport(t *testing.T) {
	type check struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	}
	type report struct {
		Checks []check `json:"checks"`
	}
	tests := []struct {
		name    string
		report  any
		want    string
		wantErr string
	}{
		{"struct", report{Checks: []check{{"daemon", true}}}, `{"schema_version":2,"checks":[{"name":"daemon","ok":true}]}` + "\n", ""},
		{"empty object", struct{}{}, `{"schema_version":2}` + "\n", ""},
		{"own schema_version", map[string]int{"schema_version": 1}, "", "has its own schema_version field"},
		{"not an object", []string{"a"}, "", "does not encode as a JSON object"},
		{"nil", nil, "", "does not encode as a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := WriteReport(&buf, tt.report)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || buf.Len() != 0 {
					t.Errorf("err = %v, wrote %q; want an error containing %q and no output", err, buf.String(), tt.wantErr)
				}
				return
			}
			if err != nil || buf.String() != tt.want {
				t.Errorf("got %q, %v; want %q", buf.String(), err, tt.want)
			}
		})
	}
}

// TestEventsHaveNoTypeField guards the NDJSON line format: an event with its
// own "type" key would produce a line with the key twice.
func TestEventsHaveNoTypeField(t *testing.T) {
	pct := 1.0
	for _, e := range []Event{
		StepStarted{ID: "a", Title: "b"}, Progress{ID: "a", Text: "b", Pct: &pct}, Log{ID: "a", Stream: StreamStdout, Line: "b"},
		Warning{ID: "a", Text: "b"}, StepDone{ID: "a", Status: StepOK},
		Result{OK: true, Data: 1, Error: &ErrorInfo{ExitCode: 1, Message: "m", Step: "s"}},
	} {
		obj, err := encodeObject(e)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(obj, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["type"]; ok {
			t.Errorf("%s has a type field", e.Type())
		}
	}
}

type failingWriter struct{ calls int }

func (w *failingWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, fmt.Errorf("write %d failed", w.calls)
}

func TestRenderersKeepTheFirstWriteError(t *testing.T) {
	type renderer interface {
		Sink
		Err() error
	}
	for name, newRenderer := range map[string]func(io.Writer) renderer{
		"plain":  func(w io.Writer) renderer { return NewPlain(w, PlainOptions{}) },
		"ndjson": func(w io.Writer) renderer { return NewNDJSON(w) },
	} {
		w := &failingWriter{}
		r := newRenderer(w)
		render(r, recorded()[:2])
		if w.calls != 2 || r.Err() == nil || r.Err().Error() != "write 1 failed" {
			t.Errorf("%s: %d writes, Err() = %v; want 2 writes and the first error", name, w.calls, r.Err())
		}
	}
}
