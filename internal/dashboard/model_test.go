package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/progress"
)

// fakeBackend answers the dashboard's reads from fields, and records them.
type fakeBackend struct {
	mu       sync.Mutex
	services []ops.StatusService
	svcErr   error
	report   *ops.StatusReport
	// logs maps a service to the lines FollowLogs writes before it waits
	// for ctx (or returns logErr).
	logs   map[string][]string
	logErr error

	deepCalls, statusCalls int
	deadline               time.Duration // the last Services call's timeout
	followed               []string
	logCtxs                []context.Context
}

func (f *fakeBackend) Services(ctx context.Context) ([]ops.StatusService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(d)
	}
	return f.services, f.svcErr
}

func (f *fakeBackend) Status(_ context.Context, deep bool) (*ops.StatusReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if deep {
		f.deepCalls++
		r := *f.report
		r.Deep = deepReport(true)
		return &r, nil
	}
	f.statusCalls++
	return f.report, nil
}

func (f *fakeBackend) FollowLogs(ctx context.Context, service string, w io.Writer) error {
	f.mu.Lock()
	f.followed = append(f.followed, service)
	f.logCtxs = append(f.logCtxs, ctx)
	lines, err := f.logs[service], f.logErr
	f.mu.Unlock()
	for _, l := range lines {
		if _, err := io.WriteString(w, l+"\n"); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func ptr[T any](v T) *T { return &v }

func deepReport(ok bool) *ops.StatusDeep {
	d := &ops.StatusDeep{
		Gateway: ops.StatusGateway{Checked: true, Healthy: ptr(ok), Status: "RUNNING", Message: "RUNNING"},
		Data:    ops.StatusData{Checked: true, Ready: ptr(ok), Message: "HPDS answered"},
		HTTP:    ops.StatusHTTP{Checked: true, CSP: ops.CSPFrontend, Message: "nonce CSP"},
	}
	if !ok {
		d.Data.Message = "HPDS health is DOWN"
	}
	return d
}

// healthyReport is a status report with every check passing.
func healthyReport() *ops.StatusReport {
	exp := time.Now().Add(365 * 24 * time.Hour)
	return &ops.StatusReport{
		Stack:      ops.StatusStack{Name: "demo", Dir: "/stacks/demo"},
		Config:     ops.StatusConfig{Valid: true},
		Versions:   ops.StatusVersions{Gate: ops.GateOK},
		Release:    ops.StatusRelease{Branch: "james_mono", Commit: "0123456789abcdef"},
		Images:     []ops.StatusImage{{Name: "hpds", Present: ptr(true)}},
		DB:         &ops.StatusDB{Mode: "local"},
		Migrations: ops.StatusMigrations{Status: "up_to_date"},
		Token:      ops.StatusToken{ExpiresAt: &exp},
		Auth0:      &ops.StatusAuth0{Needed: false},
	}
}

func keyMsg(s string) tea.KeyPressMsg {
	switch s {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	default:
		return tea.KeyPressMsg{Code: []rune(s)[0], Text: s}
	}
}

// testModel returns a sized model on a fake backend with two services and
// a healthy status already delivered.
func testModel(t *testing.T) (*model, *fakeBackend) {
	t.Helper()
	b := &fakeBackend{
		services: []ops.StatusService{
			{Service: "hpds", State: "running", Health: "healthy"},
			{Service: "psama", State: "running", Health: "starting"},
		},
		report: healthyReport(),
		logs:   map[string][]string{"hpds": {"hpds line 1", "hpds line 2"}, "psama": {"psama line"}},
	}
	m := newModel(context.Background(), t.TempDir(), b)
	t.Cleanup(m.cleanup)
	m.width, m.height = 120, 40
	m.layout()
	m, _ = update(t, m, statusMsg{report: b.report})
	return m, b
}

func update(t *testing.T, m *model, msg tea.Msg) (*model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	nm, ok := next.(*model)
	if !ok {
		t.Fatalf("Update returned %T, want *model", next)
	}
	return nm, cmd
}

// deliverServices runs a services poll and feeds its result, then the log
// follower's batches until the fake's lines for the followed service are in.
func deliverServices(t *testing.T, m *model) *model {
	t.Helper()
	m, cmd := update(t, m, pollServices(m.ctx, m.backend)())
	if cmd == nil {
		return m
	}
	b := m.backend.(*fakeBackend)
	b.mu.Lock()
	want, failing := b.logs[m.logSvc], b.logErr != nil
	b.mu.Unlock()
	if failing || len(want) == 0 {
		m, _ = update(t, m, cmd())
		return m
	}
	clean := make([]string, 0, len(want))
	for _, l := range want[max(0, len(want)-maxLogLines):] {
		clean = append(clean, progress.CleanLine(l))
	}
	return readLogs(t, m, cmd, clean...)
}

// readLogs feeds the log follower's batches until the pane ends with want.
// A batch holds only the lines already written, so under load the first one
// can be short.
func readLogs(t *testing.T, m *model, cmd tea.Cmd, want ...string) *model {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for len(m.logLines) < len(want) || !slices.Equal(m.logLines[len(m.logLines)-len(want):], want) {
		got := make(chan tea.Msg, 1)
		go func(c tea.Cmd) { got <- c() }(cmd)
		select {
		case msg := <-got:
			m, cmd = update(t, m, msg)
		case <-deadline:
			t.Fatalf("log pane holds %q, want it to end with %q", m.logLines, want)
		}
	}
	return m
}

func view(m *model) string { return ansi.Strip(m.View().Content) }

// press sends a key, feeds back the messages its commands produce (a huh
// form completes through them), and returns the RunMsg among them, if any.
func press(t *testing.T, m *model, key string) (*model, *RunMsg) {
	t.Helper()
	m, cmd := update(t, m, keyMsg(key))
	queue := []tea.Cmd{cmd}
	for i := 0; i < 50 && len(queue) > 0; i++ {
		cmd, queue = queue[0], queue[1:]
		switch msg := quickly(cmd).(type) {
		case nil:
		case RunMsg:
			return m, &msg
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			m, cmd = update(t, m, msg)
			queue = append(queue, cmd)
		}
	}
	return m, nil
}

// quickly runs cmd, giving up on one that waits (a tick or a blink).
func quickly(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

func TestServicesPollFollowsTheFirstServiceLogs(t *testing.T) {
	m, b := testModel(t)
	m = deliverServices(t, m)
	if b.deadline <= 0 || b.deadline > servicesTimeout {
		t.Errorf("compose ps ran with timeout %v, want at most %v", b.deadline, servicesTimeout)
	}
	if m.logSvc != "hpds" || !slices.Equal(m.logLines, []string{"hpds line 1", "hpds line 2"}) {
		t.Fatalf("log pane follows %q with %q", m.logSvc, m.logLines)
	}
	v := view(m)
	for _, want := range []string{"hpds", "psama", "Logs — hpds", "hpds line 2", "PIC-SURE — demo  db:local  auth:open"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
}

func TestSelectionSwitchesTheLogFollower(t *testing.T) {
	m, b := testModel(t)
	m = deliverServices(t, m)
	first := m.logSession
	m, cmd := update(t, m, keyMsg("down"))
	m, _ = update(t, m, cmd())
	if m.logSvc != "psama" || !slices.Equal(m.logLines, []string{"psama line"}) {
		t.Fatalf("after down: %q %q", m.logSvc, m.logLines)
	}
	b.mu.Lock()
	ctx := b.logCtxs[0]
	b.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Error("the first follower wasn't stopped")
	}
	// Its late lines are dropped.
	m, _ = update(t, m, logLinesMsg{sessionID: first.id, lines: []string{"stale"}})
	if slices.Contains(m.logLines, "stale") {
		t.Error("a stale session's lines reached the pane")
	}
	// Selection clamps at the ends.
	if m, cmd = update(t, m, keyMsg("down")); cmd != nil || m.selected != 1 {
		t.Errorf("down at the end moved to %d", m.selected)
	}
}

func TestPollsStayOneInFlight(t *testing.T) {
	m, _ := testModel(t)
	if cmd := m.Init(); cmd == nil || !m.pollingServices || !m.pollingStatus {
		t.Fatal("Init didn't start both polls")
	}
	if m.refreshServices() != nil || m.refreshStatus() != nil {
		t.Error("a second poll started while one was in flight")
	}
	m, _ = update(t, m, servicesMsg{})
	if m.refreshServices() == nil {
		t.Error("no poll after the last one finished")
	}
}

func TestServicesErrorShowsWhy(t *testing.T) {
	m, _ := testModel(t)
	m, _ = update(t, m, servicesMsg{err: errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock\nmore")})
	v := view(m)
	if !strings.Contains(v, "Services unavailable") || !strings.Contains(v, "Cannot connect to the Docker") {
		t.Errorf("view:\n%s", v)
	}
	m, _ = update(t, m, servicesMsg{services: []ops.StatusService{}})
	if v := view(m); !strings.Contains(v, "No containers") {
		t.Errorf("empty services:\n%s", v)
	}
}

func TestLogFollowerErrorBacksOffAndKeepsScrollback(t *testing.T) {
	m, b := testModel(t)
	m = deliverServices(t, m)
	b.mu.Lock()
	b.logErr = errors.New("no such service")
	b.logs["hpds"] = nil
	b.mu.Unlock()

	// The follower ends: a retry is scheduled after the backoff.
	m, cmd := update(t, m, logClosedMsg{sessionID: m.logSession.id})
	if cmd == nil || m.logRetryDelay != logRetryBase {
		t.Fatalf("closed: delay %v", m.logRetryDelay)
	}
	m, cmd = update(t, m, logRetryMsg{seq: m.logSeq})
	m, _ = update(t, m, cmd())
	if !m.logSession.failed.Load() {
		t.Error("a follower that failed at once isn't marked failed")
	}
	want := []string{"hpds line 1", "hpds line 2", "[log follower] no such service"}
	if !slices.Equal(m.logLines, want) {
		t.Errorf("lines = %q, want %q", m.logLines, want)
	}
	m, _ = update(t, m, logClosedMsg{sessionID: m.logSession.id})
	if m.logRetryDelay != 2*logRetryBase {
		t.Errorf("second failure: delay %v, want %v", m.logRetryDelay, 2*logRetryBase)
	}

	// A retry for a service the user left is dropped.
	if _, cmd := update(t, m, logRetryMsg{seq: m.logSeq - 1}); cmd != nil {
		t.Error("a stale retry restarted the follower")
	}

	// A follower that comes back replaces the scrollback with its tail.
	b.mu.Lock()
	b.logErr = nil
	b.logs["hpds"] = []string{"hpds line 2", "hpds line 3"}
	b.mu.Unlock()
	m, cmd = update(t, m, logRetryMsg{seq: m.logSeq})
	m = readLogs(t, m, cmd, "hpds line 2", "hpds line 3")
	if !slices.Equal(m.logLines, []string{"hpds line 2", "hpds line 3"}) {
		t.Errorf("after recovery: %q", m.logLines)
	}

	// Only a follower that ran a while resets the backoff: one that ends
	// soon after its lines (a stopped container) keeps backing off.
	m, _ = update(t, m, logClosedMsg{sessionID: m.logSession.id})
	if m.logRetryDelay != 8*time.Second {
		t.Errorf("a short session: delay %v, want 8s", m.logRetryDelay)
	}
	m, cmd = update(t, m, logRetryMsg{seq: m.logSeq})
	m, _ = update(t, m, cmd())
	m.logSession.started = time.Now().Add(-time.Minute)
	m, _ = update(t, m, logClosedMsg{sessionID: m.logSession.id})
	if m.logRetryDelay != logRetryBase {
		t.Errorf("a long session: delay %v, want %v", m.logRetryDelay, logRetryBase)
	}
}

// The selection stays on its service when the list changes, and the log
// pane follows the selection when its service goes.
func TestSelectionFollowsItsServiceAcrossPolls(t *testing.T) {
	m, _ := testModel(t)
	m = deliverServices(t, m)
	m, cmd := update(t, m, keyMsg("down"))
	m, _ = update(t, m, cmd())
	// A new service sorts before psama: the selection moves with psama.
	m, cmd = update(t, m, servicesMsg{services: []ops.StatusService{{Service: "dictionary"}, {Service: "hpds"}, {Service: "psama"}}})
	if m.selectedService() != "psama" || cmd != nil {
		t.Errorf("selected %q (cmd %v)", m.selectedService(), cmd != nil)
	}
	// psama goes: the pane follows the service now selected.
	m, cmd = update(t, m, servicesMsg{services: []ops.StatusService{{Service: "hpds"}}})
	if m.selectedService() != "hpds" || cmd == nil {
		t.Fatalf("selected %q (cmd %v)", m.selectedService(), cmd != nil)
	}
	m = readLogs(t, m, cmd, "hpds line 1", "hpds line 2")
	if m.logSvc != "hpds" {
		t.Errorf("logs follow %q: %q", m.logSvc, m.logLines)
	}
}

// A dashboard's messages don't reach one opened after it.
func TestMessagesFromAClosedDashboardAreDropped(t *testing.T) {
	old, _ := testModel(t)
	stale := old.own(func() tea.Msg { return statusTickMsg{} })()
	m, _ := testModel(t)
	if _, cmd := update(t, m, stale); cmd != nil {
		t.Error("an old dashboard's tick started a poll chain in the new one")
	}
	if _, cmd := update(t, m, m.own(func() tea.Msg { return statusTickMsg{} })()); cmd == nil {
		t.Error("the dashboard's own tick was dropped")
	}
}

func TestNextLogRetryDelay(t *testing.T) {
	d := time.Duration(0)
	var got []time.Duration
	for range 6 {
		d = nextLogRetryDelay(d)
		got = append(got, d)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("schedule = %v, want %v", got, want)
	}
}

func TestLogLinesAreCleanedAndCapped(t *testing.T) {
	m, b := testModel(t)
	b.logs["hpds"] = []string{"\x1b[31mred\x1b[0m\tline\r"}
	m = deliverServices(t, m)
	if !slices.Equal(m.logLines, []string{"red line"}) {
		t.Errorf("lines = %q", m.logLines)
	}
	flood := make([]string, maxLogLines+50)
	for i := range flood {
		flood[i] = fmt.Sprintf("line %d", i)
	}
	m, _ = update(t, m, logLinesMsg{sessionID: m.logSession.id, lines: flood})
	if len(m.logLines) != maxLogLines || m.logLines[len(m.logLines)-1] != flood[len(flood)-1] {
		t.Errorf("kept %d lines, last %q", len(m.logLines), m.logLines[len(m.logLines)-1])
	}
	if h := lipgloss.Height(m.View().Content); h > m.height {
		t.Errorf("a log flood grew the frame to %d rows", h)
	}
}

func TestDeepCheckIsCachedUntilAnAction(t *testing.T) {
	m, b := testModel(t)
	if v := view(m); !strings.Contains(v, "h checks the gateway") {
		t.Errorf("no deep-check hint:\n%s", v)
	}
	m, cmd := update(t, m, keyMsg("h"))
	if !m.deepRunning || !strings.Contains(view(m), "Health: checking") {
		t.Fatal("h didn't start the deep check")
	}
	if _, again := update(t, m, keyMsg("h")); again != nil {
		t.Error("a second h started another deep check")
	}
	// Status polls wait for it: it reports the status too.
	if m.refreshStatus() != nil {
		t.Error("a status poll started during the deep check")
	}
	m, _ = update(t, m, cmd())
	v := view(m)
	if b.deepCalls != 1 || !strings.Contains(v, "gateway healthy · data ready · CSP frontend") {
		t.Fatalf("deep calls %d, view:\n%s", b.deepCalls, v)
	}
	// Polls don't drop it.
	m, _ = update(t, m, statusMsg{report: healthyReport()})
	if m.deep == nil {
		t.Error("a status poll dropped the cached deep check")
	}

	// An action invalidates it, and a check started before the action is
	// dropped when it lands.
	m, cmd = update(t, m, keyMsg("h"))
	stale := cmd()
	m, _ = update(t, m, ActionDoneMsg{})
	if m.deep != nil || m.deepRunning {
		t.Fatal("ActionDoneMsg kept the deep check")
	}
	m, _ = update(t, m, stale)
	if m.deep != nil {
		t.Error("a deep check from before the action was shown")
	}
}

func TestDeepCheckShowsTheFirstProblem(t *testing.T) {
	m, _ := testModel(t)
	m, _ = update(t, m, deepMsg{report: healthyReport(), at: time.Now()})
	m.deep = deepReport(false)
	m.deep.Gateway.Healthy = ptr(true)
	v := view(m)
	if !strings.Contains(v, "data not ready") || !strings.Contains(v, "data: HPDS health is DOWN") {
		t.Errorf("view:\n%s", v)
	}
}

func TestActionsAskFirst(t *testing.T) {
	for _, tc := range []struct {
		key  string
		args []string
	}{
		{"r", []string{"restart", "hpds"}},
		{"u", []string{"update"}},
		{"m", []string{"migrate"}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			m, _ := testModel(t)
			m = deliverServices(t, m)
			m, run := press(t, m, tc.key)
			if run != nil || m.pending == nil || m.form == nil {
				t.Fatalf("%s didn't open a confirmation", tc.key)
			}
			// esc cancels without running.
			m, run = press(t, m, "esc")
			if run != nil || m.form != nil {
				t.Fatal("esc didn't cancel")
			}
			// Run: move to the affirmative and submit.
			m, _ = press(t, m, tc.key)
			m, _ = press(t, m, "left")
			m, run = press(t, m, "enter")
			if run == nil || !slices.Equal(run.Action.Args, tc.args) {
				t.Fatalf("run = %+v, want args %q", run, tc.args)
			}
			if m.form != nil {
				t.Error("the dialog stayed open")
			}
		})
	}
}

func TestRestartNeedsAService(t *testing.T) {
	m, _ := testModel(t)
	if m, run := press(t, m, "r"); run != nil || m.form != nil {
		t.Error("r with no services opened a dialog")
	}
}

func TestTeardownNeedsTheStackName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		keepDB bool
		args   []string
	}{
		{"reset", "R", false, []string{"--yes", "reset"}},
		{"reset keeping the db", "R", true, []string{"--yes", "reset", "--keep-db"}},
		{"destroy", "X", false, []string{"--yes", "destroy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := testModel(t)
			m, _ = press(t, m, tc.key)
			if m.teardownName != "demo" || m.form == nil {
				t.Fatal("no teardown dialog")
			}
			if v := view(m); !strings.Contains(v, "Type the stack's name, demo") {
				t.Errorf("dialog:\n%s", v)
			}
			m.keepDB = tc.keepDB

			// A wrong name doesn't run, even if the form is forced complete.
			m.confirmText = "dem"
			m.form.State = huh.StateCompleted
			m, cmd := update(t, m, struct{}{})
			if cmd != nil || m.form != nil {
				t.Fatal("a wrong name ran the action")
			}

			m, _ = press(t, m, tc.key)
			m.keepDB = tc.keepDB
			m.confirmText = "demo"
			m.status = nil // a status poll failed while the dialog was open
			m.form.State = huh.StateCompleted
			_, cmd = update(t, m, struct{}{})
			if cmd == nil {
				t.Fatal("the right name didn't run it")
			}
			run, ok := cmd().(RunMsg)
			if !ok || !slices.Equal(run.Action.Args, tc.args) {
				t.Fatalf("run = %+v, want %q", run, tc.args)
			}
		})
	}
}

func TestTeardownTypedNameIsValidated(t *testing.T) {
	m, _ := testModel(t)
	m, _ = press(t, m, "X")
	for _, r := range "nope" {
		m, _ = press(t, m, string(r))
	}
	m, run := press(t, m, "enter")
	if run != nil || m.form == nil {
		t.Fatal("a wrong typed name submitted the form")
	}
	if v := view(m); !strings.Contains(v, `type "demo" exactly`) {
		t.Errorf("no validation message:\n%s", v)
	}
}

func TestTeardownWaitsForTheStatus(t *testing.T) {
	b := &fakeBackend{report: healthyReport()}
	m := newModel(context.Background(), t.TempDir(), b)
	t.Cleanup(m.cleanup)
	m.width, m.height = 120, 40
	m, run := press(t, m, "R")
	if run != nil || m.form != nil || !strings.Contains(m.lastResult, "name isn't known") {
		t.Errorf("R before the status: form %v, result %q", m.form != nil, m.lastResult)
	}
}

// l asks the embedder to open its load wizard.
func TestLoadOpensTheWizard(t *testing.T) {
	m, _ := testModel(t)
	_, cmd := update(t, m, keyMsg("l"))
	if cmd == nil {
		t.Fatal("l sent nothing")
	}
	if _, ok := cmd().(LoadMsg); !ok {
		t.Errorf("l sent %#v, want LoadMsg", cmd())
	}
}

func TestEscLeavesAndStopsTheFollower(t *testing.T) {
	m, b := testModel(t)
	m = deliverServices(t, m)
	_, cmd := update(t, m, keyMsg("esc"))
	if _, ok := cmd().(BackMsg); !ok {
		t.Fatal("esc didn't ask to go back")
	}
	if m.ctx.Err() == nil {
		t.Error("leaving didn't cancel the dashboard's context")
	}
	b.mu.Lock()
	ctx := b.logCtxs[0]
	b.mu.Unlock()
	if ctx.Err() == nil {
		t.Error("leaving didn't stop the follower")
	}
}

func TestQuitKeyQuits(t *testing.T) {
	m, _ := testModel(t)
	_, cmd := update(t, m, keyMsg("q"))
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q didn't quit")
	}
}

// Keys that arrive in the same read as a confirm reach the dashboard
// before the embedder opens the run screen: they do nothing until it
// hands back ActionDoneMsg.
func TestNoKeyActsAfterAConfirmIsSent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		confirm func(*testing.T, *model) (*model, *RunMsg)
	}{
		{"update", func(t *testing.T, m *model) (*model, *RunMsg) {
			m, _ = press(t, m, "u")
			m, _ = press(t, m, "left")
			return press(t, m, "enter")
		}},
		{"destroy", func(t *testing.T, m *model) (*model, *RunMsg) {
			m, _ = press(t, m, "X")
			for _, r := range m.stackName() {
				m, _ = press(t, m, string(r))
			}
			return press(t, m, "enter")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := testModel(t)
			m, run := tc.confirm(t, m)
			if run == nil {
				t.Fatal("the confirm sent no RunMsg")
			}
			for _, key := range []string{"q", "ctrl+c", "esc", "l", "u", "X"} {
				var cmd tea.Cmd
				m, cmd = update(t, m, keyMsg(key))
				if cmd != nil || m.form != nil {
					t.Errorf("%s after the confirm: cmd %v, form %v", key, cmd != nil, m.form != nil)
				}
			}
			if m.ctx.Err() != nil {
				t.Error("a key after the confirm closed the dashboard")
			}
			m, _ = update(t, m, ActionDoneMsg{})
			_, cmd := update(t, m, keyMsg("l"))
			if cmd == nil {
				t.Fatal("l after ActionDoneMsg sent nothing")
			}
			if _, ok := cmd().(LoadMsg); !ok {
				t.Errorf("l after ActionDoneMsg sent %#v, want LoadMsg", cmd())
			}
		})
	}
}

func TestActionDoneRefreshes(t *testing.T) {
	m, _ := testModel(t)
	m, cmd := update(t, m, ActionDoneMsg{})
	if cmd == nil || !m.pollingServices || !m.pollingStatus {
		t.Error("ActionDoneMsg didn't poll again")
	}
}

func TestSummarySeverityFirst(t *testing.T) {
	m, _ := testModel(t)
	r := healthyReport()
	r.Migrations.Status = "pending"
	r.Images = []ops.StatusImage{{Name: "hpds", Present: ptr(false)}, {Name: "psama", Present: ptr(false)}}
	m, _ = update(t, m, statusMsg{report: r})
	lines := strings.Split(ansi.Strip(m.summaryPane()), "\n")
	var body []string
	for _, l := range lines {
		body = append(body, strings.Trim(l, "│╭╮╰╯─ "))
	}
	text := strings.Join(body, "\n")
	iBad := strings.Index(text, "✗ migrations pending")
	iWarn := strings.Index(text, "! 2 images missing")
	iOK := strings.Index(text, "✓ config · version · token")
	if iBad < 0 || iWarn < iBad || iOK < iWarn {
		t.Errorf("order wrong:\n%s", text)
	}
	if !strings.Contains(text, "release: james_mono @ 01234567") {
		t.Errorf("no release line:\n%s", text)
	}
}

func TestSummaryWarnsOfAFailedMigrationCheck(t *testing.T) {
	m, _ := testModel(t)
	r := healthyReport()
	r.Migrations = ops.StatusMigrations{Status: "unknown", Error: "Access denied for user 'root'\nmore"}
	m, _ = update(t, m, statusMsg{report: r})
	text := ansi.Strip(m.summaryPane())
	if !strings.Contains(text, "! migrations: couldn't check (Access denied for user 'root')") {
		t.Errorf("no migration warning:\n%s", text)
	}
	if strings.Contains(text, "· migrations") {
		t.Errorf("migrations listed as passing:\n%s", text)
	}
	for _, skipped := range []string{ops.MigrationsSkippedRemote, ops.MigrationsSkippedUnhealthy} {
		r.Migrations.Error = skipped
		m, _ = update(t, m, statusMsg{report: r})
		if text := ansi.Strip(m.summaryPane()); strings.Contains(text, "migrations:") {
			t.Errorf("a skipped check (%s) warns:\n%s", skipped, text)
		}
	}
}

func TestSummaryWorstCaseFitsThePane(t *testing.T) {
	m, _ := testModel(t)
	r := &ops.StatusReport{
		Stack:       ops.StatusStack{Name: "demo"},
		Config:      ops.StatusConfig{Error: "yaml: line 3: mapping values are not allowed in this context, and a long tail"},
		Versions:    ops.StatusVersions{Gate: ops.GateStackNewer},
		ImagesError: "Cannot connect to the Docker daemon",
		Migrations:  ops.StatusMigrations{Status: "pending"},
		Token:       ops.StatusToken{Error: "secrets.yaml unreadable"},
	}
	m, _ = update(t, m, statusMsg{report: r})
	m, _ = update(t, m, deepMsg{report: healthyReport(), at: time.Now()})
	m.deep = deepReport(false)
	for _, sz := range [][2]int{{80, 24}, {120, 40}} {
		m, _ = update(t, m, tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		if h := lipgloss.Height(m.summaryPane()); h != summaryHeight {
			t.Errorf("%dx%d: status pane is %d rows, want %d:\n%s", sz[0], sz[1], h, summaryHeight, ansi.Strip(m.summaryPane()))
		}
	}
}

func TestOwns(t *testing.T) {
	m, _ := testModel(t)
	if msg := m.own(servicesTick())(); !Owns(msg) {
		t.Errorf("Owns(%T) = false", msg)
	}
	for _, msg := range []tea.Msg{ActionDoneMsg{}, RunMsg{}, tea.KeyPressMsg{}, servicesMsg{}} {
		if Owns(msg) {
			t.Errorf("Owns(%T) = true", msg)
		}
	}
}

// An empty service list stops the follower; an action brings a backed-off
// follower back at once.
func TestFollowerAfterEmptyListAndAction(t *testing.T) {
	m, b := testModel(t)
	m = deliverServices(t, m)
	b.mu.Lock()
	ctx := b.logCtxs[0]
	b.mu.Unlock()
	m, _ = update(t, m, servicesMsg{services: []ops.StatusService{}})
	if m.logSvc != "" || m.logSession != nil || ctx.Err() == nil || len(m.logLines) != 0 {
		t.Errorf("empty list kept following %q", m.logSvc)
	}

	m = deliverServices(t, m)
	m, _ = update(t, m, logClosedMsg{sessionID: m.logSession.id})
	m, _ = update(t, m, logClosedMsg{}) // stale: ignored
	m.logRetryDelay = logRetryMax
	m, cmd := update(t, m, ActionDoneMsg{})
	if m.logSession == nil || m.logRetryDelay != 0 || cmd == nil {
		t.Errorf("ActionDoneMsg didn't follow again: session %v, delay %v", m.logSession != nil, m.logRetryDelay)
	}
}
