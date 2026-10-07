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
)

// runScreen runs init in-process and shows its steps with an embedded
// progress.Model (ticket 038). Ctrl-C twice cancels it; the gate's
// question opens a yes/no dialog.
type runScreen struct {
	title string
	prog  progress.Model

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

	width, height int
}

// newRunScreen starts init on req in its own goroutine.
func newRunScreen(ctx context.Context, title string, run func(context.Context, InitRequest) (InitResult, error), req InitRequest, animations bool) *runScreen {
	ctx, cancel := context.WithCancelCause(ctx)
	s := &runScreen{
		title:  title,
		msgs:   make(chan tea.Msg, 64),
		stop:   make(chan struct{}),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	s.prog = progress.New(progress.Options{
		Animations: animations,
		Interrupt:  func() { cancel(exitcode.Signaled(os.Interrupt)) },
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
	s.cancel(exitcode.Signaled(os.Interrupt))
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

func (s *runScreen) setSize(width, height int) {
	s.width, s.height = width, height
	m, _ := s.prog.Update(tea.WindowSizeMsg{Width: max(width-2, 20), Height: height})
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
		return s, s.feed(progress.DoneMsg{OK: msg.err == nil, LogPath: msg.res.LogPath})
	case tea.KeyPressMsg:
		if s.askDlg != nil {
			return s.updateAsk(msg)
		}
		if s.finished {
			if k := msg.String(); k == "enter" || k == "esc" || k == "q" {
				return s, func() tea.Msg { return runClosedMsg{} }
			}
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
	var parts []string
	var footer string
	switch {
	case s.askDlg != nil:
		parts = append(parts, s.askDlg.View())
		footer = "enter answer · esc no"
	case s.finished && s.err == nil:
		parts = append(parts, styles.OK.Render("✓ Setup finished"), strings.TrimRight(s.res.Summary, "\n"))
		footer = "enter back to the menu"
	case s.finished:
		parts = append(parts, styles.Bad.Render("✗ "+s.err.Error()))
		footer = "enter back to the menu"
	default:
		footer = "ctrl+c twice to cancel"
	}
	// The steps take what room the rest leaves, keeping their latest lines.
	tail := strings.Join(parts, "\n\n")
	room := s.height - 4 - lipgloss.Height(tail)
	steps := strings.Split(s.prog.View().Content, "\n")
	if s.height > 0 && len(steps) > max(room, 3) {
		steps = steps[len(steps)-max(room, 3):]
	}
	content := lipgloss.JoinVertical(lipgloss.Left,
		runTitleStyle.Render(s.title), strings.Join(steps, "\n"), "", tail, runFooterStyle.Render(footer))
	if s.width == 0 || s.height == 0 {
		return content
	}
	return lipgloss.Place(s.width, s.height, lipgloss.Center, lipgloss.Center, content)
}
