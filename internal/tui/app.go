package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/styles"
)

// Screen identifies the active top-level screen.
type Screen int

const (
	ScreenLanding Screen = iota
	ScreenDashboard
	ScreenWizard
	ScreenLoadData
	ScreenRun
)

// Options configures the unified TUI.
type Options struct {
	Root string
	// Untrusted, when set, is why the stack found here can't be used: it
	// belongs to another user (§6.1). The landing shows it and offers no
	// stack actions.
	Untrusted  string
	Animations bool
	// Init runs init in-process for the setup wizard and "Resume setup",
	// sending its events to req.Sink.
	Init func(ctx context.Context, req InitRequest) (InitResult, error)
	// Defaults is the config the setup wizard opens with for a new stack
	// in dir: free ports, a name. Nil means stack.DefaultConfig.
	Defaults func(dir string) stack.Config
	// Dashboard reads the stack in Root for the dashboard.
	Dashboard dashboard.Backend
	// Command runs a pic-sure command line in-process for the landing's
	// and the dashboard's actions and the load wizard's loads, sending its
	// events to req.Sink.
	Command func(ctx context.Context, req CommandRequest) (InitResult, error)
}

// CommandRequest is a command the landing, the dashboard or the load
// wizard asks Options.Command to run.
type CommandRequest struct {
	// Dir is the stack directory, which the command gets as --stack. It is
	// empty for a command that runs without one (dashboard.Action.NoStack).
	Dir string
	// Args is the rest of the command line, such as ["restart", "hpds"].
	Args []string
	// Sink receives the command's events.
	Sink events.Sink
	// Confirm asks the user a yes/no question on the run screen, for the
	// compatibility gate's self-update offer.
	Confirm func(ctx context.Context, question string) (bool, error)
}

// openWizardMsg asks the app to open the setup wizard.
type openWizardMsg struct{}

// resumeSetupMsg asks the app to run init on the stack's own config.
type resumeSetupMsg struct{}

// Run starts the unified TUI and blocks until the user quits or ctx is
// done. The CLI owns SIGINT and SIGTERM, which cancel ctx; Bubble Tea's own
// handler is off so that a signal always ends the program through ctx, with
// an error, and the CLI can exit 128+N. A forced quit from the run screen
// (a third Ctrl-C) returns an exit 130 error naming the step that was
// running, without waiting for the operation.
func Run(ctx context.Context, o Options) error {
	opts := []tea.ProgramOption{tea.WithContext(ctx), tea.WithoutSignalHandler()}
	if os.Getenv("NO_COLOR") != "" {
		// Bubble Tea's profile detection only honours NO_COLOR values that
		// parse as true; no-color.org counts any non-empty value.
		opts = append(opts, tea.WithColorProfile(colorprofile.Ascii))
	}
	a := newApp(ctx, o)
	_, err := tea.NewProgram(a, opts...).Run()
	return a.end(err)
}

// end settles the run screen's operation once the program has ended with
// err, and returns Run's error.
func (a *app) end(err error) error {
	switch {
	case a.run != nil && a.run.prog.Forced:
		// A third Ctrl-C while the operation stopped: leave it behind. The
		// process exits soon after, and the stack lock with it.
		a.run.abandon()
		return ForcedQuit(a.run.prog.Running())
	case a.run != nil:
		// Ended by a signal while init ran: let it stop and clean up.
		a.run.close()
	}
	return err
}

// ForcedQuit is the exit 130 error Run returns when the user forces a quit
// while step (or, if step is "", the operation) stops.
func ForcedQuit(step string) error {
	what := "the operation"
	if step != "" {
		what = fmt.Sprintf("step %q", step)
	}
	return &exitcode.Error{Code: exitcode.CodeInterrupted, Err: fmt.Errorf(
		"quit while %s was still stopping; containers it started may still be running. Run 'pic-sure status' to check", what)}
}

type app struct {
	ctx           context.Context
	opts          Options
	width, height int

	screen  Screen
	landing *landing
	dash    tea.Model
	// dashCancel stops the dashboard's polls and log follower.
	dashCancel context.CancelFunc
	// runCommand is set while the run screen runs a command line (an
	// action or a load) rather than init.
	runCommand bool
	wizard     *wizardScreen
	load       *loadScreen
	run        *runScreen
	// lastSetup is the wizard's last confirmed setup, kept while an init
	// of it failed before writing the stack, so Set up reopens it.
	lastSetup *wizardDoneMsg
}

func newApp(ctx context.Context, o Options) *app {
	a := &app{ctx: ctx, opts: o, screen: ScreenLanding}
	a.landing = newLanding(o.Root, a.detectStack(), o.Animations)
	a.landing.notice = o.Untrusted
	return a
}

// stackStatus is what the landing finds in its directory.
type stackStatus int

const (
	noStack stackStatus = iota
	// partStack has a pic-sure.yaml, but init hasn't finished.
	partStack
	readyStack
	// untrustedStack is a stack another user owns (Options.Untrusted).
	untrustedStack
)

// detectStack is what the landing finds in Root now.
func (a *app) detectStack() stackStatus {
	if a.opts.Untrusted != "" {
		return untrustedStack
	}
	return detectStack(a.opts.Root)
}

func detectStack(root string) stackStatus {
	if _, err := os.Stat(filepath.Join(root, stack.ConfigFile)); err != nil {
		return noStack
	}
	if st, err := stack.PeekState(root); err != nil || st == nil || st.InitializedAt.IsZero() {
		return partStack
	}
	return readyStack
}

// Init asks the terminal for its background color alongside the landing's
// startup, so the palette and the dialogs can match it.
func (a *app) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, a.landing.startAnimations())
}

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if a.run != nil && leavesScreen(msg) {
		// Sent by another screen before the run screen opened (keys that
		// arrived in one read). Acting on it would hide the run, or replace
		// it and leave its operation running unseen, holding the stack lock.
		return a, nil
	}
	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		// The palette and dialog.Theme read it on every render.
		styles.SetDarkBackground(msg.IsDark())

	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.landing.setSize(msg.Width, msg.Height)
		if a.wizard != nil {
			a.wizard.setSize(msg.Width, msg.Height)
		}
		if a.load != nil {
			a.load.setSize(msg.Width, msg.Height)
		}
		if a.run != nil {
			a.run.setSize(msg.Width, msg.Height)
		}
		if a.dash != nil {
			var cmd tea.Cmd
			a.dash, cmd = a.dash.Update(msg)
			return a, cmd
		}
		return a, nil

	// --- navigation ---
	case openDashboardMsg:
		return a.openDashboard()

	case dashboard.BackMsg:
		// The dashboard has already stopped itself; with a run open (esc
		// batched after a confirm) the run stays in front and closing it
		// returns to the landing.
		a.closeDashboard()
		if a.run != nil {
			return a, nil
		}
		return a.openLanding()

	case dashboard.RunMsg:
		return a.startAction(msg.Action)

	case dashboard.LoadMsg:
		return a.openLoad("")

	case openWizardMsg:
		defaults, sec := stack.DefaultConfig(), stack.UserSecrets{}
		if a.opts.Defaults != nil {
			defaults = a.opts.Defaults(a.opts.Root)
		}
		base := defaults
		if a.lastSetup != nil {
			if cfg, err := a.lastSetup.doc.Config(); err == nil {
				base, sec = *cfg, a.lastSetup.secrets
			}
		}
		s := newWizardScreen(defaults, base, sec)
		a.landing.stopAnimations()
		s.setSize(a.width, a.height)
		a.wizard = s
		a.screen = ScreenWizard
		return a, s.init()

	case wizardClosedMsg:
		a.wizard = nil
		a.landing.result = "setup cancelled — nothing written"
		if msg.err != nil {
			a.landing.result = "setup failed: " + msg.err.Error()
		}
		return a, a.openLandingCmd()

	case wizardDoneMsg:
		// Consent was given at the wizard's confirm-summary.
		a.wizard = nil
		a.lastSetup = &msg
		return a.startInit(InitRequest{Dir: a.opts.Root, Config: msg.doc, Secrets: msg.secrets})

	case resumeSetupMsg:
		return a.startInit(InitRequest{Dir: a.opts.Root})

	case runClosedMsg:
		if a.run == nil {
			return a, nil // a second close of a run screen already closed
		}
		if a.runCommand {
			return a.actionClosed()
		}
		a.run.close()
		failed := a.run.err != nil
		a.run = nil
		if failed && a.lastSetup != nil && a.detectStack() == noStack {
			a.landing.result = "setup failed before creating the stack; Set up has your answers"
		} else {
			a.lastSetup = nil
		}
		return a, a.openLandingCmd()

	case openLoadDataMsg:
		return a.openLoad(msg.kind)

	case loadDataClosedMsg:
		a.closeLoad()
		if a.dash != nil {
			// Opened from the dashboard, which kept running behind it.
			a.screen = ScreenDashboard
			return a, nil
		}
		if msg.aborted {
			a.landing.result = "data load cancelled"
		}
		return a, a.openLandingCmd()

	case loadRunMsg:
		a.closeLoad()
		return a.startAction(msg.act)
	}

	// The dashboard's polls and log lines reach it whichever screen shows.
	if a.dash != nil && dashboard.Owns(msg) {
		var cmd tea.Cmd
		a.dash, cmd = a.dash.Update(msg)
		return a, cmd
	}

	// --- route everything else to the active screen ---
	switch a.screen {
	case ScreenDashboard:
		if a.dash == nil {
			return a, nil
		}
		var cmd tea.Cmd
		a.dash, cmd = a.dash.Update(msg)
		return a, cmd
	case ScreenWizard:
		if a.wizard == nil {
			return a, nil
		}
		var cmd tea.Cmd
		a.wizard, cmd = a.wizard.update(msg)
		return a, cmd
	case ScreenLoadData:
		if a.load == nil {
			return a, nil
		}
		var cmd tea.Cmd
		a.load, cmd = a.load.update(msg)
		return a, cmd
	case ScreenRun:
		if a.run == nil {
			return a, nil
		}
		var cmd tea.Cmd
		a.run, cmd = a.run.update(msg)
		return a, cmd
	default:
		var cmd tea.Cmd
		a.landing, cmd = a.landing.update(msg)
		return a, cmd
	}
}

// leavesScreen reports whether msg asks the app to open another screen or
// start a run. A new case of that kind in Update belongs here too, or it
// can hide an open run.
func leavesScreen(msg tea.Msg) bool {
	switch msg.(type) {
	case openDashboardMsg, dashboard.RunMsg, dashboard.LoadMsg,
		openWizardMsg, wizardClosedMsg, wizardDoneMsg, resumeSetupMsg,
		openLoadDataMsg, loadDataClosedMsg, loadRunMsg:
		return true
	}
	return false
}

func (a *app) newDashboard() {
	ctx, cancel := context.WithCancel(a.ctx)
	b := a.opts.Dashboard
	if b == nil {
		b = noBackend{}
	}
	a.dash, a.dashCancel = dashboard.New(ctx, a.opts.Root, b), cancel
}

func (a *app) closeDashboard() {
	if a.dashCancel != nil {
		a.dashCancel()
	}
	a.dash, a.dashCancel = nil, nil
}

// openLoad opens the load screen, on kind's first step if kind is set.
// From the dashboard, the dashboard keeps running behind it.
func (a *app) openLoad(kind string) (tea.Model, tea.Cmd) {
	s := newLoadScreen(a.ctx, a.opts.Root, kind)
	a.landing.stopAnimations()
	s.setSize(a.width, a.height)
	a.load = s
	a.screen = ScreenLoadData
	return a, s.init()
}

func (a *app) closeLoad() {
	if a.load != nil {
		a.load.close()
		a.load = nil
	}
}

// startAction opens the run screen on a command line: a landing or
// dashboard action, or a load. Closing it returns to the dashboard if one
// is open, else to the landing.
func (a *app) startAction(act dashboard.Action) (tea.Model, tea.Cmd) {
	if a.opts.Command == nil {
		if a.dash != nil {
			// Nothing ran; ActionDoneMsg gives the dashboard its keys back.
			a.screen = ScreenDashboard
			var cmd tea.Cmd
			a.dash, cmd = a.dash.Update(dashboard.ActionDoneMsg{})
			return a, cmd
		}
		a.landing.result = act.Title + ": not available here"
		return a, a.openLandingCmd()
	}
	a.landing.stopAnimations()
	command := a.opts.Command
	dir := a.opts.Root
	if act.NoStack {
		dir = ""
	}
	run := func(ctx context.Context, req InitRequest) (InitResult, error) {
		return command(ctx, CommandRequest{Dir: dir, Args: act.Args, Sink: req.Sink, Confirm: req.Confirm})
	}
	a.run = newRunScreen(a.ctx, act.Title, run, InitRequest{Dir: a.opts.Root}, a.opts.Animations)
	a.run.doneText = act.Done
	a.run.setSize(a.width, a.height)
	a.runCommand = true
	a.screen = ScreenRun
	return a, a.run.init()
}

// actionClosed leaves a command's run screen: back to the dashboard, or to
// the landing when there is none or the command removed the stack.
func (a *app) actionClosed() (tea.Model, tea.Cmd) {
	a.runCommand = false
	if a.run != nil {
		a.run.close()
		a.run = nil
	}
	if a.detectStack() == noStack {
		a.closeDashboard()
		return a, a.openLandingCmd()
	}
	if a.dash == nil {
		return a, a.openLandingCmd()
	}
	a.screen = ScreenDashboard
	var cmd tea.Cmd
	a.dash, cmd = a.dash.Update(dashboard.ActionDoneMsg{})
	return a, cmd
}

func (a *app) openDashboard() (tea.Model, tea.Cmd) {
	a.landing.stopAnimations()
	a.newDashboard()
	a.screen = ScreenDashboard
	// Deliver the current size before Init so the first frame is laid out.
	var cmd tea.Cmd
	a.dash, cmd = a.dash.Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
	return a, tea.Batch(cmd, a.dash.Init())
}

// startInit opens the run screen on an init of req.
func (a *app) startInit(req InitRequest) (tea.Model, tea.Cmd) {
	if a.opts.Init == nil {
		a.landing.result = "setup failed: init isn't available here"
		return a, a.openLandingCmd()
	}
	a.landing.stopAnimations()
	a.run = newRunScreen(a.ctx, "Setting up PIC-SURE", a.opts.Init, req, a.opts.Animations)
	a.run.setSize(a.width, a.height)
	a.screen = ScreenRun
	return a, a.run.init()
}

func (a *app) openLandingCmd() tea.Cmd {
	a.screen = ScreenLanding
	a.landing.leaving = false
	a.landing.setStatus(a.detectStack())
	return a.landing.startAnimations()
}

func (a *app) openLanding() (tea.Model, tea.Cmd) {
	return a, a.openLandingCmd()
}

// View draws the active screen. The whole TUI runs on the alt screen.
func (a *app) View() tea.View {
	v := tea.NewView(a.content())
	v.AltScreen = true
	return v
}

func (a *app) content() string {
	switch a.screen {
	case ScreenDashboard:
		if a.dash != nil {
			return a.dash.View().Content
		}
	case ScreenWizard:
		if a.wizard != nil {
			return a.wizard.view()
		}
	case ScreenLoadData:
		if a.load != nil {
			return a.load.view()
		}
	case ScreenRun:
		if a.run != nil {
			return a.run.view()
		}
	}
	return a.landing.view()
}

// noBackend is the dashboard's backend when the program has none.
type noBackend struct{}

var errNoBackend = errors.New("the dashboard can't read the stack here")

func (noBackend) Services(context.Context) ([]ops.StatusService, error) { return nil, errNoBackend }

func (noBackend) Status(context.Context, bool) (*ops.StatusReport, error) { return nil, errNoBackend }

func (noBackend) FollowLogs(context.Context, string, io.Writer) error { return errNoBackend }
