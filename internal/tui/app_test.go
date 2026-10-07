package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/actions"
	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
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
	orig := startRunner
	startRunner = func(string, actions.Action, int, int) (runnerHandle, error) {
		return &fakeRunner{}, nil
	}
	t.Cleanup(func() { startRunner = orig })

	a := testApp(ScreenLanding)

	a.Update(openDashboardMsg{})
	if a.screen != ScreenDashboard || a.dash == nil {
		t.Fatal("openDashboardMsg did not open the dashboard")
	}

	a.Update(dashboard.BackMsg{})
	if a.screen != ScreenLanding || a.dash != nil {
		t.Fatal("BackMsg did not return to the landing / drop the dashboard")
	}

	a.Update(runActionMsg{act: actions.Preflight()})
	if a.screen != ScreenActivity || a.activity == nil {
		t.Fatal("runActionMsg did not open the activity screen")
	}

	a.Update(activityClosedMsg{openDashboard: true})
	if a.screen != ScreenDashboard {
		t.Fatal("activityClosedMsg{openDashboard} did not open the dashboard")
	}
	if a.activity != nil {
		t.Fatal("activity not dropped after close")
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

func TestAppLoadDataDispatchOpensActivity(t *testing.T) {
	orig := startRunner
	startRunner = func(string, actions.Action, int, int) (runnerHandle, error) {
		return &fakeRunner{}, nil
	}
	t.Cleanup(func() { startRunner = orig })

	a := testApp(ScreenLanding)
	a.Update(openLoadDataMsg{})
	if a.load == nil {
		t.Fatal("load screen not open")
	}
	// The load screen emits a runActionMsg to launch the load; the app routes it
	// to the activity screen and drops the (now-closed) load screen.
	a.Update(runActionMsg{act: actions.LoadPhenotype(actions.PhenotypeOpts{File: "pheno.csv"})})
	if a.screen != ScreenActivity || a.activity == nil {
		t.Fatal("runActionMsg from the load screen did not open the activity screen")
	}
	if a.load != nil {
		t.Error("load screen not dropped after dispatch")
	}
	if want := actions.LoadPhenotype(actions.PhenotypeOpts{}).Name; a.activity.act.Name != want {
		t.Errorf("activity action = %q, want %q", a.activity.act.Name, want)
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
