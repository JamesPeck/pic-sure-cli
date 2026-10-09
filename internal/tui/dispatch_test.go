package tui

import (
	"context"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
)

// countingApp is an app whose Options.Command reports each call on the
// returned channel and returns at once.
func countingApp(t *testing.T, root string) (*app, chan []string) {
	t.Helper()
	calls := make(chan []string, 8)
	a := newApp(context.Background(), Options{Root: root, Command: func(_ context.Context, req CommandRequest) (InitResult, error) {
		calls <- req.Args
		return InitResult{}, nil
	}})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	t.Cleanup(func() {
		if a.run != nil {
			a.run.close()
		}
	})
	return a, calls
}

// assertOneCommand waits for the first command, then gives a second one
// time to show up.
func assertOneCommand(t *testing.T, calls chan []string) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("no command ran")
	}
	select {
	case args := <-calls:
		t.Fatalf("a second command ran: %q", args)
	case <-time.After(200 * time.Millisecond):
	}
}

// loadAtConfirm opens the app's load screen on kind and walks it to its
// confirm step.
func loadAtConfirm(t *testing.T, a *app, kind string) {
	t.Helper()
	stubInspect(t, nil, nil)
	a.Update(openLoadDataMsg{kind: kind})
	s := a.load
	switch kind {
	case kindDemo:
		s, _ = completeForm(s) // dataset
		s, _ = completeForm(s) // heap
	case kindGenomic:
		s = pick(t, s, "/data/index.tsv")
		s.includeVCFDir = false
		s, _ = completeForm(s) // VCF directory? no
		s.partition = "chr1"
		s, _ = completeForm(s) // partition
		s, _ = completeForm(s) // heap
		s, _ = completeForm(s) // promote
		s, _ = completeForm(s) // profile
	default:
		s = pick(t, s, "/data/pheno.csv")
		s, _ = completeForm(s) // heap
		s, _ = completeForm(s) // dictionary: auto
	}
	if s.step != loadConfirm && s.step != loadGenomicConfirm {
		t.Fatalf("%s: step = %v, want a confirm step", kind, s.step)
	}
}

// Messages that reach a load screen after its confirm completed, before the
// app has acted on the loadRunMsg (keys, huh blink ticks), dispatch nothing.
func TestLoadConfirmDispatchesOnce(t *testing.T) {
	for _, kind := range []string{kindFile, kindDemo, kindGenomic} {
		t.Run(kind, func(t *testing.T) {
			a, _ := countingApp(t, t.TempDir())
			loadAtConfirm(t, a, kind)
			s := a.load
			s.confirmed = true
			s.form.State = huh.StateCompleted
			if _, cmd := s.update(struct{}{}); cmd == nil {
				t.Fatal("the confirm dispatched nothing")
			} else if _, ok := cmd().(loadRunMsg); !ok {
				t.Fatal("the confirm didn't send loadRunMsg")
			}
			for _, m := range []tea.Msg{struct{}{}, tea.KeyPressMsg{Code: 'y', Text: "y"}, enter} {
				if _, cmd := s.update(m); cmd != nil {
					t.Errorf("%#v after the confirm sent another command", m)
				}
			}
		})
	}
}

// The landing's direct actions, pressed again before the app has acted on
// the first, ask once.
func TestLandingDirectActionsAskOnce(t *testing.T) {
	for _, id := range []string{"preflight", "resume", "dashboard", "setup", "loaddata", "demo"} {
		t.Run(id, func(t *testing.T) {
			l := newLanding(t.TempDir(), noStack, false)
			if _, cmd := l.choose(id); cmd == nil {
				t.Fatal("no command")
			}
			if _, cmd := l.update(enter); cmd != nil {
				t.Error("a second enter asked again")
			}
		})
	}
}

// A confirmed landing dialog asks once too.
func TestLandingConfirmAsksOnce(t *testing.T) {
	l := newLanding(t.TempDir(), readyStack, false)
	l.startConfirm(dashboard.MigrateAction())
	l.confirmOK = true
	l.form.State = huh.StateCompleted
	if _, cmd := l.update(struct{}{}); cmd == nil {
		t.Fatal("the confirm asked for nothing")
	}
	for _, m := range []tea.Msg{enter, struct{}{}} {
		if _, cmd := l.update(m); cmd != nil {
			t.Error("a message after the confirm asked again")
		}
	}
}

// While a run screen is open the app acts on no other screen's request: a
// second run is refused, and the run screen stays in front.
func TestAppStartsOneRunAtATime(t *testing.T) {
	root := t.TempDir()
	inits := make(chan struct{}, 4)
	a, calls := countingApp(t, root)
	a.opts.Init = func(context.Context, InitRequest) (InitResult, error) {
		inits <- struct{}{}
		return InitResult{}, nil
	}
	a.Update(openDashboardMsg{})
	a.Update(dashboard.RunMsg{Action: preflightAction(false)})
	first := a.run
	for _, m := range []tea.Msg{
		dashboard.RunMsg{Action: preflightAction(false)},
		loadRunMsg{act: dashboard.Action{Title: "Loading", Args: []string{"data", "demo"}}},
		resumeSetupMsg{}, dashboard.LoadMsg{}, openLoadDataMsg{}, openWizardMsg{}, openDashboardMsg{},
		wizardDoneMsg{}, wizardClosedMsg{}, loadDataClosedMsg{aborted: true},
		dashboard.BackMsg{},
	} {
		if _, back := m.(dashboard.BackMsg); !back && !leavesScreen(m) {
			t.Errorf("leavesScreen(%T) = false", m)
		}
		a.Update(m)
		if a.run != first || a.screen != ScreenRun {
			t.Fatalf("%#v left the run screen: screen %v", m, a.screen)
		}
	}
	// The dashboard stopped itself before its BackMsg: it's gone, and
	// closing the run returns to the landing.
	if a.dash != nil {
		t.Error("BackMsg kept the dashboard")
	}
	assertOneCommand(t, calls)
	if len(inits) != 0 {
		t.Error("init started while a command ran")
	}
	// A second close of the same run screen changes nothing.
	a.Update(runClosedMsg{})
	screen, setup := a.screen, &wizardDoneMsg{}
	a.lastSetup = setup
	a.Update(runClosedMsg{})
	if a.screen != screen || a.lastSetup != setup {
		t.Errorf("a second close: screen %v, kept setup %v", a.screen, a.lastSetup == setup)
	}
}

// Keys that arrive in one read (a paste, SSH batching, tmux send-keys)
// through a real program start one run.
func TestBatchedKeysStartOneRun(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *app)
		keys  string
	}{
		{"load confirm yy", func(t *testing.T, a *app) { loadAtConfirm(t, a, kindDemo) }, "yy"},
		{"landing preflight", func(*testing.T, *app) {}, "j\r\r"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, calls := countingApp(t, t.TempDir())
			tc.setup(t, a)
			in, keys := io.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			p := tea.NewProgram(a, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(io.Discard),
				tea.WithoutSignalHandler(), tea.WithWindowSize(100, 40))
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = p.Run()
			}()
			go func() { _, _ = keys.Write([]byte(tc.keys)) }()
			assertOneCommand(t, calls)
			cancel()
			_ = keys.Close()
			<-done
		})
	}
}
