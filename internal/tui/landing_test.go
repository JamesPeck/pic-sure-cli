package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func keyEnter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }
func keyDownN(l *landing, n int) {
	for i := 0; i < n; i++ {
		l.update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
}

func menuIDs(m *menu) []string {
	ids := make([]string, len(m.items))
	for i, it := range m.items {
		ids[i] = it.ID
	}
	return ids
}

func menuLabels(m *menu) []string {
	labels := make([]string, len(m.items))
	for i, it := range m.items {
		labels[i] = it.Label
	}
	return labels
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLandingMenuIsContextAware(t *testing.T) {
	// Fresh checkout: Preflight stays on the main menu — it matters most
	// before/during the first install.
	fresh := newLanding("/tmp/x", noStack, false)
	want := []string{"setup", "preflight", "quit"}
	if got := menuIDs(fresh.menu); !eq(got, want) {
		t.Errorf("fresh menu = %v, want %v", got, want)
	}
	// Configured: Preflight has moved to Developer options; the main menu
	// carries "Load your data…" (the guided load screen) instead of
	// "Load demo data".
	configured := newLanding("/tmp/x", readyStack, false)
	want = []string{"dashboard", "update", "loaddata", "devmenu", "quit"}
	if got := menuIDs(configured.menu); !eq(got, want) {
		t.Errorf("configured menu = %v, want %v", got, want)
	}
	// A stack init didn't finish offers to resume it.
	part := newLanding("/tmp/x", partStack, false)
	want = []string{"resume", "devmenu", "quit"}
	if got := menuIDs(part.menu); !eq(got, want) {
		t.Errorf("partial stack menu = %v, want %v", got, want)
	}
	labels := menuLabels(configured.menu)
	if !contains(labels, "Load your data…") {
		t.Errorf("configured menu labels = %v, want one to be %q", labels, "Load your data…")
	}
	if contains(labels, "Load demo data") {
		t.Errorf("configured menu still offers %q; it moved to Developer options", "Load demo data")
	}
	if contains(labels, "Preflight check") {
		t.Errorf("configured menu still offers %q; it moved to Developer options", "Preflight check")
	}
}

// TestLandingDevMenuNoWrapAt80 guards against a dev-menu label that is wide
// enough to wrap inside the menu box at the default 80-column width: the
// selected row renders as "▸ " + label + " ◂" (label+4 cells) centered into
// menuWidth, so any label longer than menuWidth-4 wraps to a second line and
// shears the box. At width 80, menuWidth = min(max(80/3,28),80-8) = 28.
func TestLandingDevMenuNoWrapAt80(t *testing.T) {
	l := newLanding("/tmp/x", readyStack, false)
	l.setSize(80, 40)
	keyDownN(l, 3) // open the dev submenu
	l.update(keyEnter())

	// Same menuWidth formula as contentLines at width 80.
	menuWidth := min(max(l.width/3, 28), l.width-8)
	n := len(l.menu.items)

	for i := 0; i < n; i++ {
		l.menu.selected = i
		lines := strings.Split(l.menu.view(menuWidth), "\n")
		if len(lines) != n {
			t.Errorf("dev menu with item %d (%q) selected rendered %d lines, want %d (label wraps in the box)",
				i, l.menu.items[i].Label, len(lines), n)
		}
		for j, line := range lines {
			if w := lipgloss.Width(line); w > menuWidth {
				t.Errorf("dev menu line %d width %d exceeds menuWidth %d with item %d selected: %q",
					j, w, menuWidth, i, line)
			}
		}
	}
}

func maxLineWidth(s string) int {
	w := 0
	for _, line := range splitLines(s) {
		if lw := lipgloss.Width(line); lw > w {
			w = lw
		}
	}
	return w
}

func TestLandingAnimationTicksSurviveConfirmDialog(t *testing.T) {
	l := newLanding("/tmp/x", readyStack, true)
	l.setSize(80, 24)
	l.startAnimations()
	keyDownN(l, 1)       // select Update
	l.update(keyEnter()) // open its confirm dialog
	if l.form == nil {
		t.Fatal("confirm did not open")
	}
	_, cmd := l.update(starTickMsg{seq: l.star.seq})
	if cmd == nil {
		t.Fatal("starfield tick swallowed by open confirm; animation frozen")
	}
}

func TestLandingQuitKeys(t *testing.T) {
	l := newLanding("/tmp/x", readyStack, false)
	_, cmd := l.update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("q returned no command")
	}
	if msg := cmd(); msg != tea.Quit() {
		t.Fatalf("q = %#v, want tea.Quit", msg)
	}
}

// The configured main-menu "Load your data…" opens the guided load screen
// (openLoadDataMsg) — NOT the parameterless ETL picker (which moved to the dev
// submenu). It must not open a landing dialog form at all.
func TestLandingLoadDataOpensGuidedScreen(t *testing.T) {
	l := newLanding("/tmp/x", readyStack, false)
	if !contains(menuIDs(l.menu), "loaddata") {
		t.Fatalf("configured menu missing the loaddata entry: %v", menuIDs(l.menu))
	}
	_, cmd := l.choose("loaddata")
	if l.form != nil || l.pickerMake != nil {
		t.Fatal("loaddata opened a landing dialog; it must emit openLoadDataMsg instead")
	}
	if cmd == nil {
		t.Fatal("loaddata produced no command")
	}
	if _, ok := cmd().(openLoadDataMsg); !ok {
		t.Fatalf("loaddata = %#v, want openLoadDataMsg", cmd())
	}
}

// The developer menu's demo entry opens the load screen on its datasets.
func TestLandingDemoOpensTheLoadScreen(t *testing.T) {
	l := newLanding("/tmp/x", readyStack, false)
	l.dev = true
	l.rebuildMenu()
	_, cmd := l.choose("demo")
	if msg, ok := cmd().(openLoadDataMsg); !ok || msg.kind != kindDemo {
		t.Fatalf("demo = %#v, want openLoadDataMsg{kind: demo}", cmd())
	}
}

// TestLandingFooterSwitchesWhenDialogIsOpen guards the footer copy: while a
// dialog is open 'q' types into the input, so the default "q quit" hint is
// wrong. The footer must switch to a dialog-appropriate hint ("esc cancel")
// when l.form != nil, and revert to the normal hints once the dialog closes.
func TestLandingFooterSwitchesWhenDialogIsOpen(t *testing.T) {
	l := newLanding("/tmp/x", readyStack, false)
	// No dialog: normal footer.
	if got := l.footer(); got != "↑/↓ select · enter · q quit" {
		t.Errorf("no-dialog footer = %q, want hint with q quit", got)
	}
	// Open a confirm dialog (update).
	_, _ = l.choose("update")
	if l.form == nil {
		t.Fatal("update did not open a dialog")
	}
	// Dialog open: must NOT show "q quit".
	got := l.footer()
	if strings.Contains(got, "q quit") {
		t.Errorf("dialog-open footer still shows 'q quit': %q", got)
	}
	if !strings.Contains(got, "esc") {
		t.Errorf("dialog-open footer missing 'esc': %q", got)
	}
	// After esc closes the dialog the footer reverts.
	l.update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := l.footer(); got != "↑/↓ select · enter · q quit" {
		t.Errorf("after-close footer = %q, want hint with q quit", got)
	}
}

// testConfig is the stack readConfig returns in these tests: named demo,
// on the main branch, with the first dev service on.
func testConfig(t *testing.T) *stack.Config {
	t.Helper()
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	cfg.Release.Branch = "main"
	cfg.Dev.Services = []string{ops.DevList(&cfg)[0].Name}
	orig := readConfig
	readConfig = func(string) (*stack.Config, error) { return &cfg, nil }
	t.Cleanup(func() { readConfig = orig })
	return &cfg
}

func TestLandingDevSubmenu(t *testing.T) {
	l := newLanding("/tmp/x", readyStack, false)
	keyDownN(l, 3) // select devmenu
	l.update(keyEnter())
	want := []string{"preflight", "dryrun", "branch", "migrate", "demo", "dictionary", "devon", "devoff", "reset", "destroy", "back"}
	if got := menuIDs(l.menu); !eq(got, want) {
		t.Fatalf("dev submenu = %v, want %v", got, want)
	}
	l.update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := menuIDs(l.menu); len(got) != 5 {
		t.Fatalf("esc did not return to main menu: %v", got)
	}
}

// pumpApp runs cmd and feeds what it sends back to a, following the
// commands that returns.
func pumpApp(a *app, cmd tea.Cmd, depth int) {
	if depth > 10 {
		return
	}
	for _, msg := range runCmd(cmd) {
		_, next := a.Update(msg)
		pumpApp(a, next, depth+1)
	}
}

func pressApp(a *app, msgs ...tea.Msg) {
	for _, m := range msgs {
		_, cmd := a.Update(m)
		pumpApp(a, cmd, 0)
	}
}

// Every landing menu item runs the pic-sure command it names, through
// Options.Command on the run screen, once its dialog is answered.
func TestLandingActionsRunTheirCommands(t *testing.T) {
	cfg := testConfig(t)
	var devOn, devOff string
	for _, v := range ops.DevList(cfg) {
		if v.On && devOff == "" {
			devOff = v.Name
		}
	}
	devOn = ops.DevList(cfg)[0].Name
	name := typeText("demo")
	tests := []struct {
		status  stackStatus
		dev     bool
		id      string
		keys    []tea.Msg
		want    []string
		noStack bool
	}{
		{noStack, false, "preflight", nil, []string{"doctor"}, true},
		{readyStack, false, "update", []tea.Msg{left, enter}, []string{"update"}, false},
		{readyStack, true, "preflight", nil, []string{"doctor"}, false},
		{readyStack, true, "dryrun", []tea.Msg{left, enter}, []string{"update", "--dry-run"}, false},
		{readyStack, true, "branch", append(typeText("-next"), enter), []string{"config", "set", "release.branch", "main-next"}, false},
		{readyStack, true, "migrate", []tea.Msg{left, enter}, []string{"migrate"}, false},
		{readyStack, true, "dictionary", []tea.Msg{enter}, []string{"dictionary", "hydrate"}, false},
		{readyStack, true, "dictionary", []tea.Msg{down, enter}, []string{"dictionary", "weights"}, false},
		{readyStack, true, "devon", []tea.Msg{enter}, []string{"dev", "on", devOn}, false},
		{readyStack, true, "devoff", []tea.Msg{enter}, []string{"dev", "off", devOff}, false},
		{readyStack, true, "reset", append(append([]tea.Msg{enter}, name...), enter), []string{"--yes", "reset"}, false},
		{readyStack, true, "reset", append(append([]tea.Msg{down, enter}, name...), enter), []string{"--yes", "reset", "--keep-db"}, false},
		{readyStack, true, "destroy", append(name, enter), []string{"--yes", "destroy"}, false},
		{partStack, true, "migrate", []tea.Msg{left, enter}, []string{"migrate"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.id+" "+strings.Join(tc.want, " "), func(t *testing.T) {
			root := t.TempDir()
			got := make(chan CommandRequest, 1)
			a := newApp(context.Background(), Options{Root: root, Command: func(_ context.Context, req CommandRequest) (InitResult, error) {
				got <- req
				return InitResult{}, nil
			}})
			a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
			a.landing.setStatus(tc.status)
			a.landing.dev = tc.dev
			a.landing.rebuildMenu()
			_, cmd := a.landing.choose(tc.id)
			pumpApp(a, cmd, 0)
			if a.landing.form != nil {
				pumpApp(a, a.landing.form.Init(), 0)
			}
			pressApp(a, tc.keys...)
			select {
			case req := <-got:
				if !eq(req.Args, tc.want) {
					t.Errorf("args = %q, want %q", req.Args, tc.want)
				}
				if wantDir := root; tc.noStack {
					if req.Dir != "" {
						t.Errorf("dir = %q, want none", req.Dir)
					}
				} else if req.Dir != wantDir {
					t.Errorf("dir = %q, want %q", req.Dir, wantDir)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no command ran; screen %v, view:\n%s", a.screen, a.content())
			}
			if a.screen != ScreenRun {
				t.Errorf("screen = %v, want the run screen", a.screen)
			}
			a.run.close()
		})
	}
}

// Every item on every landing menu does something: navigates, opens a
// dialog or runs a command. None is a dead end.
func TestLandingEveryItemIsWired(t *testing.T) {
	testConfig(t)
	nav := map[string]bool{"quit": true, "back": true, "devmenu": true}
	for _, st := range []stackStatus{noStack, partStack, readyStack} {
		for _, dev := range []bool{false, true} {
			l := newLanding("/tmp/x", st, false)
			l.dev = dev
			l.rebuildMenu()
			for _, id := range menuIDs(l.menu) {
				if nav[id] {
					continue
				}
				m := newLanding("/tmp/x", st, false)
				m.dev = dev
				m.rebuildMenu()
				_, cmd := m.choose(id)
				switch {
				case m.form != nil:
				case cmd != nil:
					if run, ok := cmd().(dashboard.RunMsg); ok && len(run.Action.Args) == 0 {
						t.Errorf("%s runs an empty command line", id)
					}
				default:
					t.Errorf("%s (status %v, dev %v) does nothing: %q", id, st, dev, m.result)
				}
			}
		}
	}
}

// Cancelling a dialog runs nothing.
func TestLandingDialogsCancel(t *testing.T) {
	testConfig(t)
	for _, id := range []string{"update", "dryrun", "migrate", "dictionary", "devon", "devoff", "branch", "reset", "destroy"} {
		l := newLanding("/tmp/x", readyStack, false)
		l.setSize(80, 24)
		_, _ = l.choose(id)
		if l.form == nil {
			t.Fatalf("%s: no dialog", id)
		}
		_, cmd := l.update(tea.KeyPressMsg{Code: tea.KeyEscape})
		if l.form != nil || l.pending != nil || l.pickerMake != nil || l.inputMake != nil || l.teardown {
			t.Errorf("%s: esc left the dialog open", id)
		}
		if cmd != nil {
			if msg := cmd(); msg != nil {
				t.Errorf("%s: esc sent %#v", id, msg)
			}
		}
	}
	// A completed yes/no answered no, and a teardown whose typed name
	// doesn't match, run nothing.
	l := newLanding("/tmp/x", readyStack, false)
	_, _ = l.choose("update")
	l.form.State = huh.StateCompleted
	if _, cmd := l.update(struct{}{}); cmd != nil {
		t.Errorf("update answered no sent %#v", cmd())
	}
	_, _ = l.choose("destroy")
	l.confirmText = "other"
	l.form.State = huh.StateCompleted
	if _, cmd := l.update(struct{}{}); cmd != nil {
		t.Errorf("destroy with the wrong name sent %#v", cmd())
	}
}

func TestLandingDevPickers(t *testing.T) {
	cfg := testConfig(t)
	l := newLanding("/tmp/x", readyStack, false)
	l.setSize(100, 40)
	_, _ = l.choose("devon")
	view := wizardANSI.ReplaceAllString(l.form.View(), "")
	for _, v := range ops.DevList(cfg) {
		if !strings.Contains(view, v.Name) {
			t.Errorf("dev on picker lacks %s:\n%s", v.Name, view)
		}
	}
	l.closeForm()

	cfg.Dev.Services = nil
	_, _ = l.choose("devoff")
	if l.form != nil || l.result != "no service is in dev mode" {
		t.Errorf("dev off with none on: form %v, result %q", l.form != nil, l.result)
	}

	readConfig = func(string) (*stack.Config, error) { return nil, errors.New("bad yaml") }
	for _, id := range []string{"devon", "branch", "reset"} {
		l.result = ""
		_, _ = l.choose(id)
		if l.form != nil || !strings.Contains(l.result, "bad yaml") {
			t.Errorf("%s with an unreadable config: form %v, result %q", id, l.form != nil, l.result)
		}
	}
}

func TestLandingResizeReflowsOpenDialog(t *testing.T) {
	testConfig(t)
	l := newLanding("/tmp/x", readyStack, false)
	l.setSize(120, 40)
	_, _ = l.choose("reset")
	wide := maxLineWidth(l.form.View())
	l.setSize(50, 40)
	if narrow := maxLineWidth(l.form.View()); narrow >= wide {
		t.Errorf("open dialog was not reflowed on resize: wide width=%d, narrow width=%d", wide, narrow)
	}
}

func TestLandingFrameStaysInBoxWithDialogs(t *testing.T) {
	testConfig(t)
	for _, sz := range [][2]int{{60, 16}, {80, 24}, {120, 30}} {
		for _, id := range []string{"dictionary", "reset", "destroy", "devon"} {
			l := newLanding("/tmp/x", readyStack, false)
			l.setSize(sz[0], sz[1])
			_, _ = l.choose(id)
			view := l.view()
			if h := lipgloss.Height(view); h > sz[1] {
				t.Errorf("size %dx%d %s: frame height %d exceeds %d", sz[0], sz[1], id, h, sz[1])
			}
			for n, line := range strings.Split(view, "\n") {
				if w := lipgloss.Width(line); w > sz[0] {
					t.Errorf("size %dx%d %s line %d: width %d exceeds %d", sz[0], sz[1], id, n, w, sz[0])
				}
			}
		}
	}
}
