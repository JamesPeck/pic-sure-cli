package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/actions"
	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
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
