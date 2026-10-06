package dashboard

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// logSession follows one service's logs. Lines
// arrive over a channel; waitLogLine bridges them into Bubble Tea messages
// one at a time. Sessions are identified so output from a cancelled session
// (after a selection change) can be discarded.
type logSession struct {
	id     int
	cancel context.CancelFunc
	lines  chan string
	// failed is true for a stub session that could not start the follower (it
	// only ever delivers a single error line then closes). The retry loop uses
	// it to tell a real, briefly-lived session (reset the backoff) apart from a
	// hard startup failure (keep backing off).
	failed bool
}

// startLogSession returns a session that delivers one not-implemented line
// and closes, so the pane says why it is empty. Ticket 040 follows the logs
// through the compose adapter (ticket 017) instead.
func startLogSession(_, _ string, id int) *logSession {
	lines := make(chan string, 1)
	lines <- "[log follower] " + errNotImplemented.Error()
	close(lines)
	return &logSession{id: id, cancel: func() {}, lines: lines, failed: true}
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

func (s *logSession) stop() {
	s.cancel()
}
