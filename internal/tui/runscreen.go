package tui

import (
	"context"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dialog"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/progress"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// InitRequest is an init the TUI asks Options.Init to run.
type InitRequest struct {
	// Dir is the stack directory.
	Dir string
	// Config is the new stack's pic-sure.yaml, from the setup wizard; nil
	// resumes the one in Dir.
	Config *stack.ConfigDoc
	// Secrets are the secrets the wizard asked for.
	Secrets stack.UserSecrets
	// Sink receives init's events.
	Sink events.Sink
	// Confirm asks the user a yes/no question, for the compatibility
	// gate's self-update offer.
	Confirm func(ctx context.Context, question string) (bool, error)
}

// InitResult is what a finished init reports.
type InitResult struct {
	// Summary is the human summary: the URL, Auth0 URLs, next steps.
	Summary string
	// LogPath is the run's log file, if it has one.
	LogPath string
}

// runClosedMsg tells the app to leave the run screen.
type runClosedMsg struct{}

// The operation's goroutine sends these to the screen, in order, on one
// channel, so its last events arrive before runDoneMsg.
type (
	runEventMsg struct{ e events.Event }
	runAskMsg   struct {
		question string
		reply    chan<- bool
	}
	runDoneMsg struct {
		res InitResult
		err error
	}
)

var (
	runTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(styles.Brand).Padding(0, 1)
	runFooterStyle = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	logLineStyle   = lipgloss.NewStyle().Faint(true)
)

// runScreen runs an operation in-process (init, or a dashboard action) and
// shows its steps with an embedded progress.Model. Ctrl-C twice cancels it,
// and a third press while it stops quits the TUI (Forced); the gate's
// question opens a yes/no dialog.
type runScreen struct {
	title string
	// doneText is the line shown when the operation succeeds.
	doneText string
	prog     progress.Model

	msgs   chan tea.Msg
	stop   chan struct{} // closed when the screen goes, so the operation never blocks on it
	cancel context.CancelCauseFunc
	done   chan struct{} // closed when the operation returns

	ask    *runAskMsg
	askOK  bool
	askDlg *huh.Form

	finished bool
	res      InitResult
	err      error
	// scroll is the first body line shown once the user scrolls the
	// finished screen; until then (scrolled false) the body shows from
	// its result line, after as many of the steps' last lines as fit.
	scroll   int
	scrolled bool

	width, height int
}

// newRunScreen starts run on req in its own goroutine.
func newRunScreen(ctx context.Context, title string, run func(context.Context, InitRequest) (InitResult, error), req InitRequest, animations bool) *runScreen {
	ctx, cancel := context.WithCancelCause(ctx)
	s := &runScreen{
		title:    title,
		doneText: "Setup finished",
		msgs:     make(chan tea.Msg, 64),
		stop:     make(chan struct{}),
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	s.prog = progress.New(progress.Options{
		Animations: animations,
		Interrupt:  func() { cancel(exitcode.Signaled(os.Interrupt)) },
		ForceQuit:  true,
	})
	req.Sink = runSink{s}
	req.Confirm = s.confirm
	go func() {
		defer close(s.done)
		res, err := run(ctx, req)
		s.send(runDoneMsg{res: res, err: err})
	}()
	return s
}

// send hands msg to the screen, or drops it once the screen is gone.
func (s *runScreen) send(msg tea.Msg) {
	select {
	case s.msgs <- msg:
	case <-s.stop:
	}
}

// runSink is the operation's events.Sink.
type runSink struct{ s *runScreen }

func (k runSink) Emit(e events.Event) { k.s.send(runEventMsg{e}) }

// confirm is InitRequest.Confirm: it shows the question and waits for the
// answer.
func (s *runScreen) confirm(ctx context.Context, question string) (bool, error) {
	reply := make(chan bool, 1)
	select {
	case s.msgs <- runAskMsg{question: question, reply: reply}:
	case <-s.stop:
		return false, nil
	case <-ctx.Done():
		return false, context.Cause(ctx)
	}
	select {
	case yes := <-reply:
		return yes, nil
	case <-s.stop:
		return false, nil
	case <-ctx.Done():
		return false, context.Cause(ctx)
	}
}

// listen waits for the operation's next message.
func (s *runScreen) listen() tea.Msg {
	select {
	case m := <-s.msgs:
		return m
	case <-s.stop:
		return nil
	}
}

func (s *runScreen) init() tea.Cmd { return tea.Batch(s.prog.Init(), s.listen) }

// close stops the operation, if it still runs, and waits for it to return.
func (s *runScreen) close() {
	s.abandon()
	<-s.done
}

// abandon stops the operation without waiting for it, for a forced quit.
func (s *runScreen) abandon() {
	s.cancel(exitcode.Signaled(os.Interrupt))
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

// forced reports whether the user forced a quit while the operation
// stopped.
func (s *runScreen) forced() bool { return s.prog.Forced }

// body is the finished screen's scrollable lines: the steps, the result
// line and the summary, wrapped to the block, and the index of the
// result line.
func (s *runScreen) body() (lines []string, result int) {
	wrap := lipgloss.NewStyle().Width(s.blockWidth())
	lines = strings.Split(s.prog.View().Content, "\n")
	result = len(lines) + 1
	res := styles.OK.Render("✓ " + s.doneText)
	if s.err != nil {
		res = styles.Bad.Render("✗ " + s.err.Error())
	}
	lines = append(lines, "")
	lines = append(lines, strings.Split(wrap.Render(res), "\n")...)
	// A failed command can have a summary too: doctor's report.
	if sum := strings.TrimRight(s.res.Summary, "\n"); sum != "" {
		lines = append(lines, "")
		lines = append(lines, strings.Split(wrap.Render(sum), "\n")...)
	}
	return lines, result
}

// logLine is the finished screen's "Log file:" line, kept below the body.
func (s *runScreen) logLine() string {
	if s.err == nil || s.res.LogPath == "" {
		return ""
	}
	return logLineStyle.Render("Log file: " + s.res.LogPath)
}

// bodyRoom is how many body lines fit between the title and the footer
// (and the log line), with a line to spare.
func (s *runScreen) bodyRoom() int {
	room := s.height - 3
	if s.logLine() != "" {
		room -= lipgloss.Height(s.logLine())
	}
	return max(room, 1)
}

// top is the first body line to show: where the user scrolled to, else
// the result line, after as many of the steps' last lines as fit.
func (s *runScreen) top(lines []string, result int) int {
	top := min(result, len(lines)-s.bodyRoom())
	if s.scrolled {
		top = s.scroll
	}
	return min(max(top, 0), max(len(lines)-s.bodyRoom(), 0))
}

// blockWidth is the width of the screen's content. It is fixed, so the
// centered block doesn't shift as lines come and go.
func (s *runScreen) blockWidth() int { return min(max(s.width-4, 40), 100) }

func (s *runScreen) setSize(width, height int) {
	s.width, s.height = width, height
	m, _ := s.prog.Update(tea.WindowSizeMsg{Width: s.blockWidth() - 2, Height: height})
	s.prog = m.(progress.Model)
	if s.askDlg != nil {
		s.askDlg = dialog.Fit(s.askDlg, max(min(width-4, 76), 40), max(height/2, 8))
	}
}

func (s *runScreen) update(msg tea.Msg) (*runScreen, tea.Cmd) {
	switch msg := msg.(type) {
	case runEventMsg:
		return s, tea.Batch(s.feed(progress.EventMsg{Event: msg.e}), s.listen)
	case runAskMsg:
		s.ask, s.askOK = &msg, false
		s.askDlg = huh.NewForm(huh.NewGroup(huh.NewConfirm().
			Title("Update pic-sure?").
			Description(msg.question).
			Affirmative("Update").
			Negative("No").
			Value(&s.askOK)))
		s.askDlg = dialog.Fit(s.askDlg, max(min(s.width-4, 76), 40), max(s.height/2, 8))
		return s, tea.Batch(s.askDlg.Init(), s.listen)
	case runDoneMsg:
		s.finished, s.res, s.err = true, msg.res, msg.err
		// The screen draws the log line itself, below the scrolling body.
		return s, s.feed(progress.DoneMsg{OK: msg.err == nil})
	case tea.KeyPressMsg:
		if s.askDlg != nil {
			return s.updateAsk(msg)
		}
		if s.finished {
			lines, result := s.body()
			top := s.top(lines, result)
			switch msg.String() {
			case "enter", "esc", "q", "ctrl+c":
				return s, func() tea.Msg { return runClosedMsg{} }
			case "up", "k":
				top--
			case "down", "j":
				top++
			case "pgup":
				top -= s.bodyRoom()
			case "pgdown":
				top += s.bodyRoom()
			case "home":
				top = 0
			case "end":
				top = len(lines)
			default:
				return s, nil
			}
			s.scroll, s.scrolled = top, true
			s.scroll = s.top(lines, result)
			return s, nil
		}
		return s, s.feed(msg)
	}
	var cmd tea.Cmd
	if s.askDlg != nil {
		_, cmd = s.updateAsk(msg)
	}
	return s, tea.Batch(cmd, s.feed(msg))
}

// updateAsk passes msg to the gate's dialog, and answers once it's done.
// esc answers no.
func (s *runScreen) updateAsk(msg tea.Msg) (*runScreen, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "esc" {
		s.answer(false)
		return s, nil
	}
	m, cmd := s.askDlg.Update(msg)
	if f, ok := m.(*huh.Form); ok {
		s.askDlg = f
	}
	switch s.askDlg.State {
	case huh.StateCompleted:
		s.answer(s.askOK)
		return s, nil
	case huh.StateAborted:
		s.answer(false)
		return s, nil
	}
	return s, cmd
}

func (s *runScreen) answer(yes bool) {
	s.ask.reply <- yes
	s.ask, s.askDlg = nil, nil
}

func (s *runScreen) feed(msg tea.Msg) tea.Cmd {
	m, cmd := s.prog.Update(msg)
	s.prog = m.(progress.Model)
	return cmd
}

func (s *runScreen) view() string {
	if s.finished && s.askDlg == nil {
		return s.finishedView()
	}
	var tail, footer string
	switch {
	case s.askDlg != nil:
		tail = s.askDlg.View()
		footer = "enter answer · esc no"
	case s.prog.Cancelling():
		footer = "ctrl+c again to quit now"
	default:
		footer = "ctrl+c twice to cancel"
	}
	// The steps take what room the rest leaves, keeping their latest lines.
	room := s.height - 4 - lipgloss.Height(tail)
	steps := strings.Split(s.prog.View().Content, "\n")
	if s.height > 0 && len(steps) > max(room, 3) {
		steps = steps[len(steps)-max(room, 3):]
	}
	return s.place(strings.Join(steps, "\n"), "", tail, runFooterStyle.Render(footer))
}

// finishedView shows the finished screen's body from top, with the log line
// and the footer always below it.
func (s *runScreen) finishedView() string {
	lines, result := s.body()
	footer := "enter to go back"
	if s.height > 0 && len(lines) > s.bodyRoom() {
		top := s.top(lines, result)
		lines = lines[top:min(top+s.bodyRoom(), len(lines))]
		footer = "↑/↓ pgup/pgdn scroll · enter to go back"
	}
	parts := []string{strings.Join(lines, "\n")}
	if l := s.logLine(); l != "" {
		parts = append(parts, l)
	}
	return s.place(append(parts, runFooterStyle.Render(footer))...)
}

// place puts the title above parts and centers the block on the screen.
func (s *runScreen) place(parts ...string) string {
	content := lipgloss.NewStyle().Width(s.blockWidth()).Render(lipgloss.JoinVertical(lipgloss.Left,
		append([]string{runTitleStyle.Render(s.title)}, parts...)...))
	if s.width == 0 || s.height == 0 {
		return content
	}
	return lipgloss.Place(s.width, s.height, lipgloss.Center, lipgloss.Center, content)
}
