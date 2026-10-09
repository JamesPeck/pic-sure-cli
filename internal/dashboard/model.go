package dashboard

import (
	"context"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

const (
	leftWidthMin = 36 // floor: fits the services row format below
	leftWidthMax = 50 // ceiling: don't starve the logs/status panes on huge terminals
	// summaryHeight is the status pane's fixed total height, its 2 border
	// rows included. Its content (summaryPane) is at most 10 rows.
	summaryHeight = 13
	maxLogLines   = 2000
)

// leftWidth is the responsive width of the services pane: a quarter of the
// terminal, clamped to [leftWidthMin, leftWidthMax]. All pane geometry
// derives from this.
func (m *model) leftWidth() int {
	return min(max(m.width/4, leftWidthMin), leftWidthMax)
}

type model struct {
	// id tells this dashboard's messages from those of one closed before
	// it (ownMsg).
	id      int64
	ctx     context.Context
	cancel  context.CancelFunc
	root    string
	backend Backend

	width, height int

	services        []ops.StatusService
	servicesErr     error
	pollingServices bool // a compose ps poll is in flight
	status          *ops.StatusReport
	statusErr       error
	pollingStatus   bool // a status poll is in flight
	selected        int

	// The deep check (h) is cached until an action runs: deepGen counts
	// actions, so a check that started before one is dropped.
	deep        *ops.StatusDeep
	deepErr     error
	deepAt      time.Time
	deepRunning bool
	deepGen     int

	logView    viewport.Model
	logLines   []string
	logSvc     string
	logSession *logSession
	logSeq     int
	// logRetryDelay is the current follower-restart backoff (commands.go).
	logRetryDelay time.Duration
	// logReplace is set when a follower restarts: its first lines (the tail
	// again) replace the scrollback, so the pane keeps the old lines and the
	// error until then instead of flickering.
	logReplace bool

	form            *huh.Form
	pending         *Action // the yes/no dialog's action
	confirmOK       bool
	confirmText     string
	teardownName    string // the name the open teardown dialog asks for
	teardownDestroy bool
	keepDB          bool
	lastResult      string

	// sent drops keys from when a confirmed action's RunMsg goes out until
	// the embedder's ActionDoneMsg, so a key that arrived in the same read
	// can't quit, leave or load before the run screen opens.
	sent bool
}

func newModel(ctx context.Context, root string, b Backend) *model {
	ctx, cancel := context.WithCancel(ctx)
	return &model{id: instances.Add(1), ctx: ctx, cancel: cancel, root: root, backend: b}
}

var instances atomic.Int64

// ownMsg is a message one of this dashboard's commands produced. A closed
// dashboard's ticks and poll results can still arrive after a new one
// opens; the id keeps them out of it.
type ownMsg struct {
	id  int64
	msg tea.Msg
}

// own stamps cmd's message with the dashboard's id.
func (m *model) own(cmd tea.Cmd) tea.Cmd {
	id := m.id
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		return ownMsg{id: id, msg: msg}
	}
}

func (m *model) Init() tea.Cmd {
	m.pollingServices, m.pollingStatus = true, true
	return tea.Batch(m.own(pollServices(m.ctx, m.backend)), m.own(pollStatus(m.ctx, m.backend)), m.own(servicesTick()), m.own(statusTick()))
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ownMsg:
		if msg.id != m.id {
			return m, nil
		}
		return m.Update(msg.msg)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		// Re-size an open dialog to the new pane width; huh recomputes its
		// group viewport geometry only in its WindowSizeMsg handler, and this
		// branch returns without routing the resize to the form.
		if m.form != nil {
			m.form = m.sizeForm(m.form)
		}
		return m, nil

	case servicesTickMsg:
		// One poll in flight at a time: a slow docker daemon must not stack
		// a new compose ps on every tick.
		return m, tea.Batch(m.own(servicesTick()), m.refreshServices())

	case statusTickMsg:
		return m, tea.Batch(m.own(statusTick()), m.refreshStatus())

	case servicesMsg:
		m.pollingServices = false
		m.servicesErr = msg.err
		if msg.err == nil {
			// The selection stays on its service while it is listed, and
			// the log pane follows the selection.
			prev := m.selectedService()
			m.services = msg.services
			m.selected = min(m.selected, max(len(m.services)-1, 0))
			for i, s := range m.services {
				if s.Service == prev {
					m.selected = i
				}
			}
			switch svc := m.selectedService(); {
			case svc == "" && m.logSvc != "":
				m.stopLogs()
			case svc != m.logSvc:
				return m, m.followLogs(svc)
			}
		}
		return m, nil

	case statusMsg:
		m.pollingStatus = false
		m.status, m.statusErr = msg.report, msg.err
		return m, nil

	case deepMsg:
		if msg.gen != m.deepGen {
			return m, nil // an action ran since it started
		}
		m.deepRunning = false
		m.deepErr, m.deepAt = msg.err, msg.at
		m.deep = nil
		if msg.err == nil && msg.report != nil {
			m.deep = msg.report.Deep
			m.status, m.statusErr = msg.report, nil
		}
		return m, nil

	case ActionDoneMsg:
		m.sent = false
		m.invalidateDeep()
		// The action may have restarted the followed service: follow it
		// again now rather than after the backoff.
		var follow tea.Cmd
		m.logRetryDelay = 0
		if m.logSession == nil && m.logSvc != "" {
			follow = m.restartLogs(m.logSvc)
		}
		return m, tea.Batch(m.refreshServices(), m.refreshStatus(), follow)

	case logLinesMsg:
		if m.logSession == nil || msg.sessionID != m.logSession.id {
			return m, nil // stale session
		}
		if m.logReplace && !m.logSession.failed.Load() {
			m.logReplace = false
			m.logLines = nil
		}
		m.logLines = append(m.logLines, msg.lines...)
		if len(m.logLines) > maxLogLines {
			m.logLines = m.logLines[len(m.logLines)-maxLogLines:]
		}
		m.refreshLogPane()
		return m, m.own(m.logSession.waitLines())

	case logClosedMsg:
		if m.logSession == nil || msg.sessionID != m.logSession.id {
			return m, nil // stale closure
		}
		// A follower that ran a while restarts quickly; one that keeps
		// ending soon after it starts (an error, a stopped container) is
		// restarted ever less often.
		if time.Since(m.logSession.started) >= logRanLong {
			m.logRetryDelay = 0
		}
		m.logSession = nil
		if m.logSvc == "" {
			return m, nil
		}
		m.logRetryDelay = nextLogRetryDelay(m.logRetryDelay)
		return m, m.own(logRetry(m.logSeq, m.logRetryDelay))

	case logRetryMsg:
		// Restart the follower for the still-current service. Ignore a tick for
		// a service the user has since switched away from (logSeq bumps on every
		// switch) or one already restarted.
		if msg.seq != m.logSeq || m.logSession != nil || m.logSvc == "" {
			return m, nil
		}
		return m, m.restartLogs(m.logSvc)

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	// Forms consume every message type while active (spinners, blinks, ...).
	if m.form != nil {
		return m.updateForm(msg)
	}
	return m, nil
}

func (m *model) refreshServices() tea.Cmd {
	if m.pollingServices {
		return nil
	}
	m.pollingServices = true
	return m.own(pollServices(m.ctx, m.backend))
}

// refreshStatus starts a status poll unless one, or a deep check, is in
// flight.
func (m *model) refreshStatus() tea.Cmd {
	if m.pollingStatus || m.deepRunning {
		return nil
	}
	m.pollingStatus = true
	return m.own(pollStatus(m.ctx, m.backend))
}

// invalidateDeep drops the cached deep check, and any still running.
func (m *model) invalidateDeep() {
	m.deepGen++
	m.deep, m.deepErr, m.deepAt, m.deepRunning = nil, nil, time.Time{}, false
}

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.sent {
		return m, nil
	}
	if m.form != nil {
		// huh ships its esc binding disabled; the help line advertises
		// "esc cancel".
		if msg.String() == "esc" {
			m.closeForm()
			return m, nil
		}
		return m.updateForm(msg)
	}

	m.lastResult = ""
	switch msg.String() {
	case "q", "ctrl+c":
		m.cleanup()
		return m, tea.Quit
	case "esc":
		m.cleanup()
		return m, func() tea.Msg { return BackMsg{} }
	case "up", "k":
		return m, m.moveSelection(-1)
	case "down", "j":
		return m, m.moveSelection(1)
	case "pgup", "pgdown", "home", "end":
		var cmd tea.Cmd
		m.logView, cmd = m.logView.Update(msg)
		return m, cmd
	case "h":
		if m.deepRunning {
			return m, nil
		}
		m.deepRunning = true
		return m, m.own(probeDeep(m.ctx, m.backend, m.deepGen))
	case "r":
		if svc := m.selectedService(); svc != "" {
			return m.startConfirm(restartAction(svc))
		}
		return m, nil
	case "u":
		return m.startConfirm(UpdateAction())
	case "m":
		return m.startConfirm(MigrateAction())
	case "l":
		return m, func() tea.Msg { return LoadMsg{} }
	case "R":
		return m.startTeardown(false)
	case "X":
		return m.startTeardown(true)
	}
	return m, nil
}

func (m *model) moveSelection(delta int) tea.Cmd {
	if len(m.services) == 0 {
		return nil
	}
	next := min(max(m.selected+delta, 0), len(m.services)-1)
	if next == m.selected {
		return nil
	}
	m.selected = next
	return m.followLogs(m.services[m.selected].Service)
}

func (m *model) selectedService() string {
	if m.selected < len(m.services) {
		return m.services[m.selected].Service
	}
	return ""
}

// stackName is the name reset and destroy ask the user to type.
func (m *model) stackName() string {
	if m.status == nil {
		return ""
	}
	return m.status.Stack.Name
}

// followLogs switches the log pane to a service, replacing any live session.
// A deliberate switch clears the scrollback and resets the restart backoff.
func (m *model) followLogs(service string) tea.Cmd {
	m.logLines = nil
	m.logRetryDelay = 0
	m.logReplace = false
	m.logView.SetContent("")
	return m.startLogs(service)
}

// restartLogs reopens the follower for the same service after it ended,
// keeping the scrollback until the new session's first lines replace it.
func (m *model) restartLogs(service string) tea.Cmd {
	m.logReplace = true
	return m.startLogs(service)
}

func (m *model) startLogs(service string) tea.Cmd {
	if m.logSession != nil {
		m.logSession.stop()
	}
	m.logSeq++
	m.logSvc = service
	m.logSession = startLogSession(m.ctx, m.backend, service, m.logSeq)
	return m.own(m.logSession.waitLines())
}

// stopLogs stops following logs, when no service is listed.
func (m *model) stopLogs() {
	if m.logSession != nil {
		m.logSession.stop()
		m.logSession = nil
	}
	m.logSeq++ // drops a pending retry
	m.logSvc = ""
	m.logLines = nil
	m.logView.SetContent("")
}

// cleanup stops the polls and the log follower when the dashboard closes.
func (m *model) cleanup() {
	if m.logSession != nil {
		m.logSession.stop()
	}
	m.cancel()
}
