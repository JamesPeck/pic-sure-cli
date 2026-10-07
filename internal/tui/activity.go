package tui

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JamesPeck/pic-sure-cli/internal/actions"
	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

var (
	activityTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(styles.Brand).Padding(0, 1)
	activityPaneStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	activityHelpStyle  = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	// Status footers reuse the shared palette (ANSI 2/3/1) with the screen's
	// bold+padded chrome.
	activityOKStyle   = lipgloss.NewStyle().Foreground(styles.StatusOK).Bold(true).Padding(0, 1)
	activityBadStyle  = lipgloss.NewStyle().Foreground(styles.StatusBad).Bold(true).Padding(0, 1)
	activityWarnStyle = lipgloss.NewStyle().Foreground(styles.StatusWarn).Bold(true).Padding(0, 1)
)

// activityClosedMsg tells the app to leave the activity screen.
type activityClosedMsg struct{ openDashboard bool }

type activityTickMsg struct{}

// abortGracePeriod is how long a confirmed abort waits for the child to honor
// the ctrl-c SIGINT before the screen offers a force-kill.
const abortGracePeriod = 10 * time.Second

// activityKillGraceMsg fires abortGracePeriod after a confirmed abort. If the
// child still hasn't exited, the footer offers the force-kill escalation. seq
// identifies the activity whose abort armed the timer: a tick armed by run A
// can outlive A's screen and be routed to a later activity B by the app — the
// stamp makes B discard it instead of cutting its own grace short (the same
// guard the dashboard pane uses).
type activityKillGraceMsg struct{ seq int }

// activitySeq numbers activity screens so stale grace ticks are identifiable.
// Only touched from the bubbletea update goroutine (newActivity is called from
// the app's Update), so a plain int is race-free.
var activitySeq int

// runnerHandle is a running action, so tests can substitute a fake.
type runnerHandle interface {
	WaitData() tea.Cmd
	Resize(rows, cols int)
	Interrupt()
	Kill()
}

// startRunner is a seam: tests replace it with a fake runner. Until ticket
// 038 renders in-process operations here, every action fails to start.
var startRunner = func(_ string, act actions.Action, _, _ int) (runnerHandle, error) {
	return nil, actions.NotImplemented(act)
}

// activity is the full-screen runner for one menu-launched action.
type activity struct {
	root string
	act  actions.Action

	runner runnerHandle
	vp     viewport.Model
	out    *actions.OutputBuffer

	width, height int
	started       time.Time
	elapsed       time.Duration

	confirmingAbort bool
	aborted         bool
	killOffered     bool // grace period elapsed, child still running: offer K
	seq             int  // this run's activitySeq; stamps grace ticks so stale ones are discarded
	done            bool
	code            int
	err             error
}

func newActivity(root string, act actions.Action) *activity {
	activitySeq++
	return &activity{root: root, act: act, out: actions.NewOutputBuffer(), started: time.Now(), seq: activitySeq}
}

// start launches the script; on start failure the screen opens directly in
// its failed state (the footer shows the error).
func (a *activity) start() tea.Cmd {
	rows, cols := a.paneSize()
	runner, err := startRunner(a.root, a.act, rows, cols)
	if err != nil {
		a.done, a.err = true, err
		return nil
	}
	a.runner = runner
	return tea.Batch(runner.WaitData(), a.tickElapsed())
}

func (a *activity) tickElapsed() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return activityTickMsg{} })
}

func (a *activity) setSize(width, height int) {
	a.width, a.height = width, height
	rows, cols := a.paneSize()
	a.vp.SetWidth(cols)
	a.vp.SetHeight(rows)
	a.refreshContent() // re-wrap at the new width
	if a.runner != nil {
		a.runner.Resize(rows, cols)
	}
}

// refreshContent re-renders the sanitized scrollback into the viewport,
// hard-wrapped at its width (ANSI-aware) so long script lines can never
// overflow the pane and shear the border. Autoscroll only when already
// tailing, so users can scroll back during a long run.
func (a *activity) refreshContent() {
	content := a.out.String()
	if a.vp.Width() > 0 {
		content = ansi.Hardwrap(content, a.vp.Width(), true)
	}
	atBottom := a.vp.AtBottom()
	a.vp.SetContent(content)
	if atBottom {
		a.vp.GotoBottom()
	}
}

func (a *activity) paneSize() (rows, cols int) {
	return max(a.height-6, 5), max(a.width-6, 20)
}

func (a *activity) update(msg tea.Msg) (*activity, tea.Cmd) {
	switch msg := msg.(type) {
	case actions.OutputMsg:
		a.out.Feed(msg.Data)
		a.refreshContent()
		if a.runner != nil {
			return a, a.runner.WaitData()
		}
		return a, nil

	case actions.DoneMsg:
		a.done, a.code, a.err = true, msg.Code, msg.Err
		a.runner = nil
		// Completion beats a pending abort question: dismiss it so 'y' can't
		// claim an abort of a run that already finished. It also cancels a
		// pending kill-grace offer — the child exited on its own.
		a.confirmingAbort = false
		a.killOffered = false
		a.elapsed = time.Since(a.started).Round(time.Second)
		return a, nil

	case activityKillGraceMsg:
		// The grace period after a confirmed abort elapsed. Discard a stale
		// tick armed by a previous activity screen, then — if the child still
		// hasn't exited — surface the force-kill escalation in the footer.
		if msg.seq != a.seq {
			return a, nil
		}
		if a.aborted && !a.done {
			a.killOffered = true
		}
		return a, nil

	case activityTickMsg:
		if a.done {
			return a, nil
		}
		a.elapsed = time.Since(a.started).Round(time.Second)
		return a, a.tickElapsed()

	case tea.KeyPressMsg:
		return a.handleKey(msg)
	}
	return a, nil
}

func (a *activity) handleKey(msg tea.KeyPressMsg) (*activity, tea.Cmd) {
	key := msg.String()

	if a.confirmingAbort {
		switch key {
		case "y", "Y", "ctrl+c": // ctrl+c is a reflexive second press — treat as "yes, abort"
			a.confirmingAbort = false
			if a.done {
				return a, nil // run finished while confirming; nothing to abort
			}
			a.aborted = true
			if a.runner != nil {
				a.runner.Interrupt()
			}
			// Start the grace timer: if the child ignores the SIGINT, the
			// footer will offer a force-kill once it elapses.
			return a, a.killGrace()
		case "n", "N", "esc":
			a.confirmingAbort = false
		}
		return a, nil
	}

	if !a.done {
		switch key {
		case "K":
			// Escalation: only live once the grace period elapsed with the
			// child still running (footer advertises it then).
			if a.killOffered && a.runner != nil {
				a.runner.Kill()
			}
			return a, nil
		case "esc", "ctrl+c":
			a.confirmingAbort = true
			return a, nil
		case "pgup", "pgdown", "up", "down", "home", "end":
			var cmd tea.Cmd
			a.vp, cmd = a.vp.Update(msg)
			return a, cmd
		}
		return a, nil
	}

	// done
	switch key {
	case "enter":
		if a.code == 0 && a.err == nil && !a.aborted {
			return a, func() tea.Msg { return activityClosedMsg{openDashboard: true} }
		}
	case "esc", "q":
		return a, func() tea.Msg { return activityClosedMsg{} }
	case "ctrl+c":
		// The child has already exited, nothing is at risk: honor the
		// universal quit reflex instead of swallowing it.
		return a, tea.Quit
	case "pgup", "pgdown", "up", "down", "home", "end":
		var cmd tea.Cmd
		a.vp, cmd = a.vp.Update(msg)
		return a, cmd
	}
	return a, nil
}

// killGrace schedules the force-kill offer for abortGracePeriod after a
// confirmed abort, stamped with this run's seq so a later screen ignores it.
func (a *activity) killGrace() tea.Cmd {
	seq := a.seq
	return tea.Tick(abortGracePeriod, func(time.Time) tea.Msg { return activityKillGraceMsg{seq: seq} })
}

func (a *activity) view() string {
	title := activityTitleStyle.Render(a.headerLine())
	// Lip Gloss v2 counts the border in Width; the pane is a.width-4 inside it.
	inner := max(a.width-4, 22)
	pane := activityPaneStyle.Width(inner + activityPaneStyle.GetHorizontalBorderSize()).Render(a.vp.View())
	return lipgloss.JoinVertical(lipgloss.Left, title, pane, a.footerLine())
}

func (a *activity) headerLine() string {
	if a.done {
		return fmt.Sprintf("%s — finished", a.act.Name)
	}
	return fmt.Sprintf("%s — running %s", a.act.Name, a.elapsed)
}

func (a *activity) footerLine() string {
	switch {
	case a.confirmingAbort:
		return activityWarnStyle.Render(fmt.Sprintf("⚠ %s is still running — abort it? (y/n)", a.act.Name))
	case a.killOffered && !a.done:
		// Grace period elapsed and the child is still alive — escalate.
		return activityWarnStyle.Render("child ignoring interrupt — K: force kill") +
			activityHelpStyle.Render("  pgup/pgdn scroll")
	case a.aborted && !a.done:
		return activityWarnStyle.Render("aborting — sent ctrl-c, waiting for the child to exit…") +
			activityHelpStyle.Render("  pgup/pgdn scroll")
	case !a.done:
		return activityHelpStyle.Render("esc/ctrl+c abort · pgup/pgdn scroll")
	case a.aborted:
		// "[icon status] — [next action]" phrasing (U10).
		return activityWarnStyle.Render("⚠ aborted — "+a.act.AbortNote) +
			activityHelpStyle.Render("  esc/q: menu")
	case a.err != nil:
		return activityBadStyle.Render(fmt.Sprintf("✗ failed to start — %v", a.err)) +
			activityHelpStyle.Render("  esc/q: menu")
	case a.code != 0:
		return activityBadStyle.Render(fmt.Sprintf("✗ exited %d — esc/q: menu · pgup/pgdn scroll", a.code))
	default:
		return activityOKStyle.Render(fmt.Sprintf("✓ done in %s — enter: dashboard · esc/q: menu", a.elapsed))
	}
}
