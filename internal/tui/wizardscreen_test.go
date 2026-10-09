package tui

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

var wizardANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// runCmd runs c and returns its messages, flattening batches and
// sequences. A command that doesn't return within 50ms (a blink or
// animation tick) is dropped.
func runCmd(c tea.Cmd) []tea.Msg {
	if c == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- c() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(50 * time.Millisecond):
		return nil
	}
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, sub := range b {
			out = append(out, runCmd(sub)...)
		}
		return out
	}
	// tea.Sequence's message is an unexported []tea.Cmd.
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
		var out []tea.Msg
		for i := range v.Len() {
			out = append(out, runCmd(v.Index(i).Interface().(tea.Cmd))...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// drive feeds msgs to s, following the commands it returns, and returns
// the messages meant for the app.
func drive(s *wizardScreen, msgs ...tea.Msg) []tea.Msg {
	var out []tea.Msg
	queue := append([]tea.Msg{}, msgs...)
	for i := 0; i < 500 && len(queue) > 0; i++ {
		m := queue[0]
		queue = queue[1:]
		switch m.(type) {
		case wizardDoneMsg, wizardClosedMsg:
			out = append(out, m)
			continue
		case tea.WindowSizeMsg:
			continue
		}
		_, cmd := s.update(m)
		next := runCmd(cmd)
		// Keys typed later must come after what this one caused.
		queue = append(next, queue...)
	}
	return out
}

func typeText(s string) []tea.Msg {
	var out []tea.Msg
	for _, r := range s {
		out = append(out, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return out
}

var (
	enter = tea.KeyPressMsg{Code: tea.KeyEnter}
	down  = tea.KeyPressMsg{Code: tea.KeyDown}
	left  = tea.KeyPressMsg{Code: tea.KeyLeft}
	esc   = tea.KeyPressMsg{Code: tea.KeyEscape}
)

func testWizard(t *testing.T) *wizardScreen {
	t.Helper()
	base := stack.DefaultConfig()
	base.Name = "demo"
	base.Network.HTTPPort, base.Network.HTTPSPort = 8080, 8443
	s := newWizardScreen(base, base, stack.UserSecrets{})
	s.setSize(100, 60)
	drive(s, runCmd(s.init())...)
	return s
}

func seq(parts ...any) []tea.Msg {
	var out []tea.Msg
	for _, p := range parts {
		switch p := p.(type) {
		case []tea.Msg:
			out = append(out, p...)
		case tea.Msg:
			out = append(out, p)
		}
	}
	return out
}

// An open-mode setup: no Auth0 page, no client secret asked for, and the
// confirmed config carries what was entered.
func TestWizardOpenModeSetup(t *testing.T) {
	s := testWizard(t)
	out := drive(s, seq(
		enter, enter, enter, // name, release branch, theme
		down, enter, // auth mode: open
		typeText("admin@example.com"), enter,
		enter, enter, // ports
		enter, // database: local
		enter, // HPDS options
		enter, // no proxy
	)...)
	if len(out) != 0 || s.phase != wizardConfirm {
		t.Fatalf("phase = %v, out = %v; want the confirm-summary", s.phase, out)
	}
	view := wizardANSI.ReplaceAllString(s.view(), "")
	for _, want := range []string{"Create the stack", "admin@example.com", "open", "8443"} {
		if !strings.Contains(view, want) {
			t.Errorf("summary lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Auth0") {
		t.Errorf("open-mode summary lists Auth0:\n%s", view)
	}
	out = drive(s, left, enter)
	if len(out) != 1 {
		t.Fatalf("confirm gave %v, want one wizardDoneMsg", out)
	}
	done, ok := out[0].(wizardDoneMsg)
	if !ok {
		t.Fatalf("confirm gave %T, want wizardDoneMsg", out[0])
	}
	cfg, err := done.doc.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "demo" || cfg.Auth.Mode != stack.AuthOpen || cfg.Auth.AdminEmail != "admin@example.com" || cfg.Network.HTTPSPort != 8443 {
		t.Errorf("config = %+v", cfg)
	}
	if done.secrets.Auth0ClientSecret != "" {
		t.Error("open mode supplied a client secret")
	}
	// Terminal phase: a late message can't confirm twice.
	if _, cmd := s.update(enter); cmd != nil {
		t.Error("a message after consent produced another command")
	}
}

// In required mode the Auth0 page asks for the secret, and a short one
// doesn't get past it.
func TestWizardRequiredModeAsksForTheClientSecret(t *testing.T) {
	s := testWizard(t)
	drive(s, seq(
		enter, enter, enter,
		enter, // auth mode: required
		typeText("admin@example.com"), enter,
		enter,                  // tenant
		typeText("cid"), enter, // client ID
		typeText("short"), enter,
	)...)
	view := wizardANSI.ReplaceAllString(s.view(), "")
	if !strings.Contains(view, "PSAMA needs at least 32") {
		t.Fatalf("a short client secret was accepted:\n%s", view)
	}
	if strings.Contains(view, "short") {
		t.Errorf("the secret is echoed:\n%s", view)
	}
}

func TestWizardEscClosesPristineAndAsksWhenDirty(t *testing.T) {
	s := testWizard(t)
	if out := drive(s, esc); len(out) != 1 {
		t.Fatalf("esc on a pristine form gave %v, want wizardClosedMsg", out)
	}

	s = testWizard(t)
	drive(s, seq(enter, enter, enter, enter, typeText("a@example.com"))...)
	if out := drive(s, esc); len(out) != 0 || !s.discarding {
		t.Fatalf("esc on a dirty form closed it (out %v)", out)
	}
	if !strings.Contains(wizardANSI.ReplaceAllString(s.view(), ""), "Discard setup?") {
		t.Error("the discard question isn't shown")
	}
	drive(s, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if s.discarding {
		t.Fatal("n didn't withdraw the question")
	}
	drive(s, esc)
	if out := drive(s, tea.KeyPressMsg{Code: 'y', Text: "y"}); len(out) != 1 {
		t.Fatalf("y gave %v, want wizardClosedMsg", out)
	}
}

// Ctrl-C asks as esc does, instead of huh aborting the form.
func TestWizardCtrlCAsksWhenDirty(t *testing.T) {
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	s := testWizard(t)
	if out := drive(s, ctrlC); len(out) != 1 {
		t.Fatalf("ctrl+c on a pristine form gave %v, want wizardClosedMsg", out)
	}

	s = testWizard(t)
	drive(s, seq(enter, enter, enter, enter, typeText("a@example.com"))...)
	if out := drive(s, ctrlC); len(out) != 0 || !s.discarding {
		t.Fatalf("ctrl+c on a dirty form closed it (out %v)", out)
	}
	drive(s, ctrlC)
	if s.discarding || s.wf.Value("auth.admin_email") != "a@example.com" {
		t.Fatal("a second ctrl+c didn't withdraw the question, keeping the answers")
	}
}

func TestWizardConfirmCancelWritesNothing(t *testing.T) {
	s := testWizard(t)
	drive(s, seq(enter, enter, enter, down, enter, typeText("admin@example.com"), enter, enter, enter, enter, enter, enter)...)
	out := drive(s, enter) // the focused button is Cancel
	if len(out) != 1 {
		t.Fatalf("cancel gave %v", out)
	}
	if m, ok := out[0].(wizardClosedMsg); !ok || m.err != nil {
		t.Fatalf("cancel gave %#v, want wizardClosedMsg", out[0])
	}
}
