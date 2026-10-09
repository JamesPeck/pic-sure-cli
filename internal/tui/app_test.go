package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func testApp(start Screen) *app {
	a := newApp(context.Background(), Options{Root: "/tmp/x", Start: start, Animations: false})
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return a
}

func TestAppStartsOnRequestedScreen(t *testing.T) {
	if a := testApp(ScreenLanding); a.screen != ScreenLanding {
		t.Errorf("start screen = %v, want landing", a.screen)
	}
	a := testApp(ScreenDashboard)
	if a.screen != ScreenDashboard || a.dash == nil {
		t.Error("ScreenDashboard start did not construct the dashboard")
	}
}

func TestAppNavigationCycle(t *testing.T) {
	a := testApp(ScreenLanding)

	a.Update(openDashboardMsg{})
	if a.screen != ScreenDashboard || a.dash == nil {
		t.Fatal("openDashboardMsg did not open the dashboard")
	}

	a.Update(dashboard.BackMsg{})
	if a.screen != ScreenLanding || a.dash != nil {
		t.Fatal("BackMsg did not return to the landing / drop the dashboard")
	}
}

func TestAppDropsStarfieldTicksOffLanding(t *testing.T) {
	a := testApp(ScreenLanding)
	a.Update(openDashboardMsg{})
	if _, cmd := a.Update(starTickMsg{seq: 1}); cmd != nil {
		t.Error("starfield tick rescheduled while off the landing screen")
	}
}

func TestWizardFlowResultMessages(t *testing.T) {
	t.Run("cancel shows neutral result", func(t *testing.T) {
		a := testApp(ScreenLanding)
		a.screen = ScreenWizard
		a.Update(wizardClosedMsg{})
		if a.screen != ScreenLanding || !strings.Contains(a.landing.result, "nothing written") {
			t.Fatalf("screen=%v result=%q, want landing with cancelled message", a.screen, a.landing.result)
		}
	})
	t.Run("an unusable setup returns to landing with error", func(t *testing.T) {
		a := testApp(ScreenLanding)
		a.screen = ScreenWizard
		a.Update(wizardClosedMsg{err: errors.New("bad name")})
		if a.screen != ScreenLanding || !strings.Contains(a.landing.result, "setup failed: bad name") {
			t.Fatalf("screen=%v result=%q, want landing with failure", a.screen, a.landing.result)
		}
	})
}

func TestAppLoadDataNavigation(t *testing.T) {
	a := testApp(ScreenLanding)

	// openLoadDataMsg constructs and routes to the guided load screen.
	a.Update(openLoadDataMsg{})
	if a.screen != ScreenLoadData || a.load == nil {
		t.Fatalf("openLoadDataMsg did not open the load screen (screen=%v load=%v)", a.screen, a.load != nil)
	}

	// A cancel closes back to the landing with the neutral result message.
	a.Update(loadDataClosedMsg{aborted: true})
	if a.screen != ScreenLanding || a.load != nil {
		t.Fatal("loadDataClosedMsg did not return to the landing / drop the load screen")
	}
	if !strings.Contains(a.landing.result, "cancelled") {
		t.Errorf("cancel result = %q, want a cancelled message", a.landing.result)
	}
}

// A load runs its command on the run screen; closing it returns to the
// landing it was opened from.
func TestAppLoadRunsOnTheRunScreen(t *testing.T) {
	root := t.TempDir()
	var got CommandRequest
	a := newApp(context.Background(), Options{
		Root: root,
		Command: func(_ context.Context, req CommandRequest) (InitResult, error) {
			got = req
			return InitResult{Summary: "Loaded."}, nil
		},
	})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	a.Update(openLoadDataMsg{})
	act := dashboard.Action{Title: "Loading phenotype data", Done: "Phenotype data loaded", Args: []string{"data", "demo", "nhanes"}}
	a.Update(loadRunMsg{act: act})
	if a.screen != ScreenRun || a.run == nil || a.load != nil {
		t.Fatalf("loadRunMsg: screen %v run %v load %v", a.screen, a.run != nil, a.load != nil)
	}
	<-a.run.done
	for !a.run.finished {
		a.Update(a.run.listen())
	}
	if got.Dir != root || strings.Join(got.Args, " ") != "data demo nhanes" {
		t.Errorf("command = %+v", got)
	}
	if v := a.content(); !strings.Contains(v, "Phenotype data loaded") || !strings.Contains(v, "Loading phenotype data") {
		t.Errorf("run screen:\n%s", v)
	}
	a.Update(runClosedMsg{})
	if a.screen != ScreenLanding || a.run != nil {
		t.Errorf("closing: screen %v", a.screen)
	}
}

// Without a Command, a load says so on the landing instead of hanging.
func TestAppLoadWithoutCommand(t *testing.T) {
	a := testApp(ScreenLanding)
	a.Update(openLoadDataMsg{})
	a.Update(loadRunMsg{act: dashboard.Action{Title: "Loading phenotype data"}})
	if a.screen != ScreenLanding || !strings.Contains(a.landing.result, "not available") {
		t.Errorf("screen %v result %q", a.screen, a.landing.result)
	}
}

// l on the dashboard opens the load screen over it; cancelling returns to
// the dashboard, and a load's run screen does too.
func TestDashboardLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, stack.ConfigFile), []byte("schema: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newApp(context.Background(), Options{
		Root: root, Start: ScreenDashboard,
		Command: func(context.Context, CommandRequest) (InitResult, error) { return InitResult{}, nil },
	})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	a.Update(dashboard.LoadMsg{})
	if a.screen != ScreenLoadData || a.load == nil || a.dash == nil {
		t.Fatalf("LoadMsg: screen %v", a.screen)
	}
	a.Update(loadDataClosedMsg{aborted: true})
	if a.screen != ScreenDashboard || a.load != nil {
		t.Fatalf("cancel: screen %v", a.screen)
	}
	a.Update(dashboard.LoadMsg{})
	a.Update(loadRunMsg{act: dashboard.Action{Title: "Loading", Args: []string{"data", "demo"}}})
	<-a.run.done
	for !a.run.finished {
		a.Update(a.run.listen())
	}
	a.Update(runClosedMsg{})
	if a.screen != ScreenDashboard || a.dash == nil {
		t.Errorf("after the load: screen %v", a.screen)
	}
}

func TestOpenWizardUsesDefaults(t *testing.T) {
	a := newApp(context.Background(), Options{Root: "/tmp/x", Defaults: func(dir string) stack.Config {
		c := stack.DefaultConfig()
		c.Name = "from-" + filepath.Base(dir)
		return c
	}})
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	a.Update(openWizardMsg{})
	if a.screen != ScreenWizard || a.wizard == nil {
		t.Fatal("openWizardMsg did not open the wizard screen")
	}
	if got := a.wizard.wf.Value("name"); got != "from-x" {
		t.Errorf("wizard name = %q, want the Defaults one", got)
	}
}

// A setup whose init failed before creating the stack reopens with its
// answers, secrets included; a finished one doesn't.
func TestFailedSetupKeepsItsAnswers(t *testing.T) {
	root := t.TempDir()
	fail := errors.New("the host isn't ready")
	a := newApp(context.Background(), Options{Root: root, Init: func(context.Context, InitRequest) (InitResult, error) {
		return InitResult{}, fail
	}})
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	cfg := stack.DefaultConfig()
	cfg.Name = "kept"
	cfg.Auth.AdminEmail = "admin@example.com"
	cfg.Auth.Auth0.ClientID = "cid"
	doc, err := stack.NewConfigDoc(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Update(wizardDoneMsg{doc: doc, secrets: stack.UserSecrets{Auth0ClientSecret: "kept-secret"}})
	<-a.run.done
	a.run.err = fail
	a.Update(runClosedMsg{})
	if !strings.Contains(a.landing.result, "Set up has your answers") {
		t.Errorf("result = %q", a.landing.result)
	}
	a.Update(openWizardMsg{})
	if a.wizard.wf.Value("name") != "kept" || a.wizard.wf.Value("auth.auth0.client_secret") != "kept-secret" {
		t.Errorf("the wizard reopened with name %q, secret set %v", a.wizard.wf.Value("name"), a.wizard.wf.Value("auth.auth0.client_secret") != "")
	}

	a.Update(wizardDoneMsg{doc: doc})
	<-a.run.done
	a.run.err = nil
	a.Update(runClosedMsg{})
	if a.lastSetup != nil {
		t.Error("a finished setup was kept")
	}
}

// A dashboard action runs on the run screen, and closing it returns to the
// dashboard, which polls again; the dashboard's own messages reach it while
// the run screen shows.
func TestDashboardActionRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, stack.ConfigFile), []byte("schema: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got CommandRequest
	a := newApp(context.Background(), Options{
		Root: root, Start: ScreenDashboard,
		Command: func(_ context.Context, req CommandRequest) (InitResult, error) {
			got = req
			req.Sink.Emit(events.StepStarted{ID: "restart", Title: "Restart hpds"})
			req.Sink.Emit(events.StepDone{ID: "restart", Status: events.StepOK})
			return InitResult{}, nil
		},
	})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	a.Update(dashboard.RunMsg{Action: dashboard.Action{Title: "Restarting hpds", Done: "Restarted hpds", Args: []string{"restart", "hpds"}}})
	if a.screen != ScreenRun || a.run == nil || a.dash == nil {
		t.Fatal("RunMsg didn't open the run screen over the dashboard")
	}
	<-a.run.done
	for !a.run.finished {
		a.Update(a.run.listen())
	}
	if got.Dir != root || strings.Join(got.Args, " ") != "restart hpds" {
		t.Errorf("command = %+v", got)
	}
	if v := a.content(); !strings.Contains(v, "Restarted hpds") || !strings.Contains(v, "Restart hpds") {
		t.Errorf("run screen:\n%s", v)
	}

	_, cmd := a.Update(runClosedMsg{})
	if a.screen != ScreenDashboard || a.run != nil || cmd == nil {
		t.Fatalf("closing didn't return to the dashboard (screen %v)", a.screen)
	}
}

// An action that removed the stack (destroy) returns to the landing.
func TestDashboardActionThatRemovedTheStack(t *testing.T) {
	a := newApp(context.Background(), Options{
		Root: t.TempDir(), Start: ScreenDashboard,
		Command: func(context.Context, CommandRequest) (InitResult, error) { return InitResult{}, nil },
	})
	a.Update(dashboard.RunMsg{Action: dashboard.Action{Title: "Destroying the stack", Args: []string{"--yes", "destroy"}}})
	a.Update(runClosedMsg{})
	if a.screen != ScreenLanding || a.dash != nil {
		t.Errorf("screen %v, dashboard kept %v", a.screen, a.dash != nil)
	}
}

// A dashboard action's command asks through the run screen's dialog.
func TestDashboardActionAsksOnTheRunScreen(t *testing.T) {
	answer := make(chan bool, 1)
	a := newApp(context.Background(), Options{
		Root: t.TempDir(), Start: ScreenDashboard,
		Command: func(ctx context.Context, req CommandRequest) (InitResult, error) {
			yes, err := req.Confirm(ctx, "release-control abc needs pic-sure 2.1.0; this is pic-sure 2.0.0. Update pic-sure now?")
			answer <- yes
			return InitResult{}, err
		},
	})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	a.Update(dashboard.RunMsg{Action: dashboard.Action{Title: "Updating PIC-SURE", Args: []string{"update"}}})
	s := a.run
	defer s.close()
	pumpRun(t, s, func() bool { return s.askDlg != nil })
	if v := a.content(); !strings.Contains(v, "needs pic-sure 2.1.0") {
		t.Fatalf("the question isn't shown:\n%s", v)
	}
	feedRun(s, runCmd(s.askDlg.Init())...)
	feedRun(s, left, enter)
	select {
	case yes := <-answer:
		if !yes {
			t.Error("Update answered no")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no answer reached the command; view:\n%s", a.content())
	}
}

// A third Ctrl-C on the run screen quits the TUI without waiting for an
// operation that ignores cancellation, with exit 130 and the step's name.
func TestRunScreenForceQuitEndsRun(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	a := newApp(context.Background(), Options{
		Root: t.TempDir(), Start: ScreenDashboard,
		Command: func(_ context.Context, req CommandRequest) (InitResult, error) {
			req.Sink.Emit(events.StepStarted{ID: "images", Title: "Build the images"})
			<-release
			return InitResult{}, nil
		},
	})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	a.Update(dashboard.RunMsg{Action: dashboard.Action{Title: "Building", Args: []string{"build"}}})
	s := a.run
	pumpRun(t, s, func() bool { return strings.Contains(plainView(s), "Build the images") })
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	a.Update(ctrlC)
	a.Update(ctrlC)
	_, cmd := a.Update(ctrlC)
	if cmd == nil {
		t.Fatal("a third Ctrl-C didn't quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("a third Ctrl-C didn't quit")
	}
	err := a.end(nil)
	var coded *exitcode.Error
	if !errors.As(err, &coded) || coded.Code != exitcode.CodeInterrupted {
		t.Fatalf("end = %v, want exit 130", err)
	}
	for _, want := range []string{`"Build the images"`, "may still be running", "pic-sure status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// A stack another user owns shows why it can't be used and offers no stack
// actions, even though the directory holds a stack, and stays that way when
// the landing reopens.
func TestAppUntrustedStack(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, stack.ConfigFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	msg := "stack belongs to another user: owned by mallory"
	a := newApp(context.Background(), Options{Root: root, Untrusted: msg})
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	check := func(when string) {
		t.Helper()
		if got, want := menuIDs(a.landing.menu), []string{"preflight", "quit"}; !eq(got, want) {
			t.Errorf("%s: menu = %v, want %v", when, got, want)
		}
		if v := a.landing.view(); !strings.Contains(v, "mallory") {
			t.Errorf("%s: landing doesn't show the notice:\n%s", when, v)
		}
	}
	check("at start")
	a.openLandingCmd()
	check("after reopening")
}
