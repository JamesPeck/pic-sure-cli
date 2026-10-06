package events

import (
	"bytes"
	"sync"
)

// Sink receives events. Operations emit from several goroutines at once (a
// subprocess's stdout and stderr are copied concurrently), so every Sink
// must be safe for concurrent use.
type Sink interface {
	Emit(Event)
}

// SinkFunc adapts a function to a Sink. The function must be safe for
// concurrent use.
type SinkFunc func(Event)

func (f SinkFunc) Emit(e Event) { f(e) }

// Discard is a Sink that drops every event.
var Discard Sink = SinkFunc(func(Event) {})

// Recorder is a Sink that keeps every event, for tests.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *Recorder) Emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Events returns a copy of the events emitted so far, in order.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Types returns the Type of each event emitted so far, in order.
func (r *Recorder) Types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	types := make([]string, len(r.events))
	for i, e := range r.events {
		types[i] = e.Type()
	}
	return types
}

// maxLineLen bounds a LogWriter's buffer: a longer line without a newline is
// emitted in pieces.
const maxLineLen = 64 * 1024

// LogWriter is an io.Writer that turns subprocess output into Log events,
// one per line, so it can be handed to docker.Runner.Stream. A trailing \r
// is dropped from each line. Close emits a final unterminated line.
type LogWriter struct {
	sink   Sink
	id     string
	stream string

	mu  sync.Mutex
	buf []byte
}

// NewLogWriter returns a LogWriter that emits Log{ID: id, Stream: stream}.
func NewLogWriter(sink Sink, id, stream string) *LogWriter {
	return &LogWriter{sink: sink, id: id, stream: stream}
}

func (w *LogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	for len(w.buf) >= maxLineLen {
		w.emit(w.buf[:maxLineLen])
		w.buf = w.buf[maxLineLen:]
	}
	return len(p), nil
}

// Close emits any buffered partial line. It never fails.
func (w *LogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
	return nil
}

func (w *LogWriter) emit(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	w.sink.Emit(Log{ID: w.id, Stream: w.stream, Line: string(line)})
}
