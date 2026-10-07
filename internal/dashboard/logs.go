package dashboard

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/progress"
)

// logSession follows one service's logs. Lines arrive over a channel;
// waitLines bridges them into Bubble Tea messages. Sessions are identified
// so output from a cancelled session (after a selection change) can be
// discarded.
type logSession struct {
	id      int
	started time.Time
	cancel  context.CancelFunc
	lines   chan string
	// failed is set when the follower ended with an error before it
	// delivered a line. Its error line then doesn't replace the scrollback.
	failed atomic.Bool
}

// startLogSession follows service's logs through b until ctx is done or the
// session is stopped. An error ends the session with one line saying why.
func startLogSession(ctx context.Context, b Backend, service string, id int) *logSession {
	ctx, cancel := context.WithCancel(ctx)
	s := &logSession{id: id, started: time.Now(), cancel: cancel, lines: make(chan string, 256)}
	w := &lineWriter{ctx: ctx, lines: s.lines}
	go func() {
		defer close(s.lines)
		err := b.FollowLogs(ctx, service, w)
		w.flush()
		if err != nil && ctx.Err() == nil {
			if !w.hasWritten() {
				s.failed.Store(true)
			}
			w.send("[log follower] " + err.Error())
		}
	}()
	return s
}

// lineWriter turns FollowLogs' output into cleaned lines on a channel. A
// send blocks while the dashboard is behind, until the session is stopped.
type lineWriter struct {
	ctx   context.Context
	lines chan<- string

	mu    sync.Mutex
	buf   []byte
	wrote bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		w.wrote = true
		if !w.send(line) {
			return len(p), w.ctx.Err()
		}
	}
	return len(p), nil
}

func (w *lineWriter) hasWritten() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wrote
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.wrote = true
		w.send(string(w.buf))
		w.buf = nil
	}
}

func (w *lineWriter) send(line string) bool {
	select {
	case w.lines <- progress.CleanLine(line):
		return true
	case <-w.ctx.Done():
		return false
	}
}

// waitLines blocks for the next line, then drains everything else already
// buffered so a log flood costs one viewport rebuild per batch instead of
// one per line. If the channel closes mid-drain the batch is still
// delivered; the next call reports the closure.
func (s *logSession) waitLines() tea.Cmd {
	return func() tea.Msg {
		line, ok := <-s.lines
		if !ok {
			return logClosedMsg{sessionID: s.id}
		}
		batch := []string{line}
		for len(batch) < maxLogLines {
			select {
			case l, ok := <-s.lines:
				if !ok {
					return logLinesMsg{sessionID: s.id, lines: batch}
				}
				batch = append(batch, l)
			default:
				return logLinesMsg{sessionID: s.id, lines: batch}
			}
		}
		return logLinesMsg{sessionID: s.id, lines: batch}
	}
}

// stop ends the follower. A pending waitLines still returns: the channel
// closes once FollowLogs does.
func (s *logSession) stop() {
	s.cancel()
}
