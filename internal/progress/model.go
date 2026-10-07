// Package progress is the TUI renderer for an operation's events (spec
// §10.3): an inline list of steps with a spinner and status, a bounded tail
// of the running step's log lines, and warnings. Renderer runs it as an
// inline Bubble Tea program that is an events.Sink; Model is the Bubble Tea
// model itself, for a screen that runs operations in-process to embed.
package progress

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

const (
	// LiveTail is how many of the running step's log lines show.
	LiveTail = 8
	// FailTail is how many of a failed step's log lines stay on screen.
	FailTail = 20
	// confirmTimeout is how long the "press Ctrl-C again" prompt waits.
	confirmTimeout = 5 * time.Second
)

var (
	faint     = lipgloss.NewStyle().Faint(true)
	titleBold = lipgloss.NewStyle().Bold(true)
	spinStyle = lipgloss.NewStyle().Foreground(styles.Brand)
)

// Options configures a Model.
type Options struct {
	// Animations turns the spinner on; without it a running step shows a
	// static mark (--no-animations).
	Animations bool
	// Interrupt is called when the user confirms Ctrl-C. It should cancel
	// the operation's context; the model keeps rendering the events the
	// operation emits while it stops. A further Ctrl-C while it stops is a
	// force quit: the model sets Forced and quits, as a second SIGINT
	// would kill the process.
	Interrupt func()
	// NoColor strips color from what scrollback prints, which Bubble Tea
	// writes as is, whatever the program's color profile.
	NoColor bool
	// Scrollback prints each finished step above the program with
	// tea.Println, so a long list stays in the terminal's scrollback and
	// the live area holds only what is still running. An inline program
	// wants it; a screen embedding the model in the alt-screen doesn't.
	Scrollback bool
}

// EventMsg delivers one operation event to the model.
type EventMsg struct{ Event events.Event }

// DoneMsg ends the run: the model shows what is left, plus the log file's
// path when the run failed, and quits when it is a program of its own.
type DoneMsg struct {
	OK bool
	// LogPath is the run's log file, if it has one.
	LogPath string
}

// printMsg prints a line above the program (Renderer's log writer).
type printMsg struct{ text string }

// printedMsg says the queued lines have been printed.
type printedMsg struct{}

// confirmExpiredMsg withdraws the Ctrl-C prompt it was armed for.
type confirmExpiredMsg struct{ seq int }

// row is one line of the list: a step, or a warning no step owns.
type row struct {
	step     bool
	id       string
	title    string
	status   events.StepStatus // "" while running
	progress string
	pct      *float64
	warnings []string
	tail     []string // the last FailTail log lines
}

func (r *row) addLog(line string) {
	r.tail = append(r.tail, line)
	if len(r.tail) > FailTail {
		r.tail = r.tail[len(r.tail)-FailTail:]
	}
}

func (r *row) running() bool { return r.step && r.status == "" }

// Model renders an operation's events. Feed it EventMsg and end it with
// DoneMsg.
type Model struct {
	opts Options
	rows []*row
	byID map[string]*row
	// other holds progress and log lines whose ID names no step, when no
	// step is running to show them under.
	other row

	// committed counts the leading rows that scrollback has taken out of
	// the view. pending is scrollback text waiting to print; printing is
	// set while a print is in flight, since only one may be, to keep order.
	committed int
	pending   []string
	printing  bool
	// printHook sees every print, for tests.
	printHook func(string)

	spin       spinner.Model
	width      int
	confirm    bool
	confirmSeq int
	cancelling bool
	// Forced is set when the user forced a quit while the operation was
	// stopping.
	Forced bool

	done    bool
	ok      bool
	logPath string
}

// New returns a model with no steps yet.
func New(opts Options) Model {
	return Model{
		opts: opts,
		byID: map[string]*row{},
		spin: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(spinStyle)),
	}
}

// Init starts the spinner. It doesn't ask for the terminal's background:
// a short run could exit before the reply, which the shell would then read.
func (m Model) Init() tea.Cmd {
	if m.opts.Animations {
		return m.spin.Tick
	}
	return nil
}

// Done reports whether the model has had its DoneMsg.
func (m Model) Done() bool { return m.done }

// Update handles events, Ctrl-C, the spinner and the window size.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case EventMsg:
		m.apply(msg.Event)
		return m, m.flush()
	case printMsg:
		m.pending = append(m.pending, msg.text)
		return m, m.flush()
	case printedMsg:
		m.printing = false
		if cmd := m.flush(); cmd != nil || !m.done {
			return m, cmd
		}
		return m, tea.Quit
	case DoneMsg:
		m.done, m.ok, m.logPath = true, msg.OK, msg.LogPath
		m.confirm = false
		if !m.opts.Scrollback {
			return m, nil
		}
		// Print every row, so none is clipped from a final frame taller
		// than the terminal; quit once the prints are done.
		if cmd := m.flush(); cmd != nil || m.printing {
			return m, cmd
		}
		return m, tea.Quit
	case tea.KeyPressMsg:
		return m.key(msg)
	case confirmExpiredMsg:
		if msg.seq == m.confirmSeq {
			m.confirm = false
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case spinner.TickMsg:
		if m.opts.Animations && !m.done {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

// key handles Ctrl-C: the first press asks to confirm, the second cancels
// the operation, and one more while it stops forces a quit. Any other key
// withdraws the prompt.
func (m Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	if m.cancelling {
		if msg.String() == "ctrl+c" && m.opts.Scrollback {
			m.Forced = true
			return m, tea.Quit
		}
		return m, nil
	}
	if msg.String() != "ctrl+c" {
		m.confirm = false
		return m, nil
	}
	if !m.confirm {
		m.confirm = true
		m.confirmSeq++
		seq := m.confirmSeq
		return m, tea.Tick(confirmTimeout, func(time.Time) tea.Msg { return confirmExpiredMsg{seq} })
	}
	m.confirm, m.cancelling = false, true
	if m.opts.Interrupt != nil {
		m.opts.Interrupt()
	}
	return m, nil
}

func (m *Model) apply(e events.Event) {
	switch e := e.(type) {
	case events.StepStarted:
		r := &row{step: true, id: e.ID, title: cleanLine(e.Title)}
		m.rows = append(m.rows, r)
		m.byID[e.ID] = r
	case events.StepDone:
		if r := m.byID[e.ID]; r != nil {
			r.status = e.Status
		}
	case events.Progress:
		r := m.owner(e.ID)
		r.progress, r.pct = cleanLine(e.Text), e.Pct
	case events.Log:
		m.owner(e.ID).addLog(cleanLine(e.Line))
	case events.Warning:
		text := cleanLine(e.Text)
		if r := m.byID[e.ID]; r != nil && r.running() {
			r.warnings = append(r.warnings, text)
		} else {
			m.rows = append(m.rows, &row{title: text})
		}
	}
}

// owner is the row a progress or log event belongs to: its step while that
// step runs, else the last step still running, else other.
func (m *Model) owner(id string) *row {
	if r := m.byID[id]; r != nil && r.running() {
		return r
	}
	for i := len(m.rows) - 1; i >= 0; i-- {
		if m.rows[i].running() {
			return m.rows[i]
		}
	}
	return &m.other
}

// flush moves finished leading rows (every row, once done) to the print
// queue and starts printing it, in scrollback mode. Only one print is in
// flight at a time, so lines can't overtake each other.
func (m *Model) flush() tea.Cmd {
	if !m.opts.Scrollback {
		return nil
	}
	for m.committed < len(m.rows) && (m.done || !m.rows[m.committed].running()) {
		m.pending = append(m.pending, m.renderRow(m.rows[m.committed]))
		m.committed++
	}
	if m.printing || len(m.pending) == 0 {
		return nil
	}
	text := strings.Join(m.pending, "\n")
	if m.opts.NoColor {
		text = ansi.Strip(text)
	}
	m.pending = nil
	m.printing = true
	if m.printHook != nil {
		m.printHook(text)
	}
	return tea.Sequence(tea.Println(text), func() tea.Msg { return printedMsg{} })
}

// View is the live area: the rows not yet printed, the running steps' log
// tails, and the Ctrl-C prompt. Once done it is what stays on screen: the
// rest of the list, a failed step's last log lines, and the log file.
func (m Model) View() tea.View {
	var b strings.Builder
	line := func(s string) {
		b.WriteString(m.fit(s))
		b.WriteByte('\n')
	}
	for _, p := range m.pending {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	for _, r := range m.rows[m.committed:] {
		b.WriteString(m.renderRow(r))
		b.WriteByte('\n')
	}
	if !m.done && (m.other.progress != "" || len(m.other.tail) > 0) {
		if m.other.progress != "" {
			line("  " + faint.Render(progressText(&m.other)))
		}
		for _, l := range lastN(m.other.tail, LiveTail) {
			line("  " + faint.Render("│ "+l))
		}
	}
	switch {
	case m.done && !m.ok && m.logPath != "":
		line(faint.Render("Log file: " + m.logPath))
	case m.cancelling && m.opts.Scrollback:
		line(styles.Warn.Render("Cancelling: waiting for the current step to stop… (Ctrl-C again to quit now)"))
	case m.cancelling:
		line(styles.Warn.Render("Cancelling: waiting for the current step to stop…"))
	case m.confirm:
		line(styles.Warn.Render("Press Ctrl-C again to cancel."))
	}
	return tea.NewView(strings.TrimSuffix(b.String(), "\n"))
}

// renderRow renders a row with its warnings and, while running or after
// failing, its log tail.
func (m Model) renderRow(r *row) string {
	var lines []string
	if !r.step {
		return m.fit(styles.Warn.Render("! " + r.title))
	}
	head := m.mark(r) + " " + r.title
	switch r.status {
	case "":
		if p := progressText(r); p != "" {
			head += faint.Render(" · " + p)
		}
	case events.StepSkipped:
		head = faint.Render("- " + r.title + " (skipped)")
	case events.StepFailed:
		head = m.mark(r) + " " + titleBold.Render(r.title)
	}
	lines = append(lines, m.fit(head))
	for _, w := range r.warnings {
		lines = append(lines, m.fit("  "+styles.Warn.Render("! "+w)))
	}
	n := 0
	switch r.status {
	case "":
		n = LiveTail
	case events.StepFailed:
		n = FailTail
	}
	for _, l := range lastN(r.tail, n) {
		lines = append(lines, m.fit("  "+faint.Render("│ "+l)))
	}
	return strings.Join(lines, "\n")
}

func (m Model) mark(r *row) string {
	switch r.status {
	case events.StepOK:
		return styles.OK.Render("✓")
	case events.StepFailed:
		return styles.Bad.Render("✗")
	case events.StepSkipped:
		return faint.Render("-")
	}
	if m.opts.Animations && !m.done {
		return m.spin.View()
	}
	return spinStyle.Render("•")
}

// fit truncates s to the window width.
func (m Model) fit(s string) string {
	if m.width <= 0 {
		return s
	}
	return ansi.Truncate(s, m.width, "…")
}

func progressText(r *row) string {
	if r.pct == nil || math.IsNaN(*r.pct) || math.IsInf(*r.pct, 0) {
		return r.progress
	}
	pct := min(max(*r.pct, 0), 100)
	if r.progress == "" {
		return fmt.Sprintf("%.0f%%", pct)
	}
	return fmt.Sprintf("%s (%.0f%%)", r.progress, pct)
}

func lastN(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// CleanLine is cleanLine, for screens that draw other command output.
func CleanLine(s string) string { return cleanLine(s) }

// cleanLine makes event text safe to draw: the text after its last carriage
// return (a progress bar's final state), with escape sequences and other
// control characters (C1 included) removed and tabs expanded.
func cleanLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return -1
		}
		return r
	}, s)
}
