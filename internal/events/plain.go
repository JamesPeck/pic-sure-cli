package events

import (
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"
)

// PlainOptions configures a Plain renderer.
type PlainOptions struct {
	// Color wraps each status marker in an ANSI colour. The cli layer turns
	// it off for NO_COLOR, TERM=dumb and output that isn't a terminal.
	Color bool
	// Now is the timestamp source; nil means time.Now.
	Now func() time.Time
}

// Plain is the Sink for plain output (spec §10.3): one timestamped line per
// event, with a status marker, for a log or a terminal without the TUI.
//
//	15:04:05 [ .. ] Start the database
//	15:04:06        | a line of subprocess output
//	15:04:07 [ 42%] 3/11 images
//	15:04:08 [WARN] build-spec has no PSCLI entry
//	15:04:09 [ OK ] Start the database
//
// StepDone repeats the title its StepStarted gave. Result renders nothing:
// the cli layer reports a failed command itself.
type Plain struct {
	mu     sync.Mutex
	w      io.Writer
	color  bool
	now    func() time.Time
	titles map[string]string // step ID → title, from StepStarted
}

// NewPlain returns a Plain renderer writing to w, which is usually stderr.
// Write errors are ignored.
func NewPlain(w io.Writer, opts PlainOptions) *Plain {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Plain{w: w, color: opts.Color, now: now, titles: map[string]string{}}
}

// Status markers. Each is six columns wide, so the text after them lines up.
const (
	markRunning = "[ .. ]"
	markOK      = "[ OK ]"
	markSkipped = "[SKIP]"
	markFailed  = "[FAIL]"
	markWarning = "[WARN]"
	markNone    = "      "
)

// ANSI SGR codes for the markers.
const (
	sgrCyan   = "36"
	sgrGreen  = "32"
	sgrFaint  = "2"
	sgrYellow = "33"
	sgrRed    = "1;31"
)

func (p *Plain) Emit(e Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e := e.(type) {
	case StepStarted:
		p.titles[e.ID] = e.Title
		p.line(markRunning, sgrCyan, e.Title)
	case Progress:
		if pct, ok := finitePct(e.Pct); ok {
			p.line(fmt.Sprintf("[%3.0f%%]", math.Min(math.Max(pct, 0), 100)), sgrCyan, e.Text)
		} else {
			p.line(markRunning, sgrCyan, e.Text)
		}
	case Log:
		text := "|"
		if e.Line != "" {
			text += " " + e.Line
		}
		p.line(markNone, "", text)
	case Warning:
		p.line(markWarning, sgrYellow, e.Text)
	case StepDone:
		title, ok := p.titles[e.ID]
		if !ok {
			title = e.ID
		}
		delete(p.titles, e.ID)
		switch e.Status {
		case StepOK:
			p.line(markOK, sgrGreen, title)
		case StepSkipped:
			p.line(markSkipped, sgrFaint, title)
		default:
			p.line(markFailed, sgrRed, title)
		}
	}
}

// line writes text after a timestamp and mark. Each extra line of a
// multi-line text gets the timestamp and a blank mark, so every output line
// stays timestamped and aligned.
func (p *Plain) line(mark, sgr, text string) {
	ts := p.now().Format(time.TimeOnly)
	if p.color && sgr != "" {
		mark = "\x1b[" + sgr + "m" + mark + "\x1b[0m"
	}
	var b strings.Builder
	for i, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if i > 0 {
			mark = markNone
		}
		s := ts + " " + mark
		if l == "" {
			s = strings.TrimRight(s, " ")
		} else {
			s += " " + l
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	_, _ = io.WriteString(p.w, b.String())
}

// finitePct returns *pct when it is set and finite. A NaN or infinite
// percentage (a 0/0 somewhere) is treated as unknown.
func finitePct(pct *float64) (float64, bool) {
	if pct == nil || math.IsNaN(*pct) || math.IsInf(*pct, 0) {
		return 0, false
	}
	return *pct, true
}
