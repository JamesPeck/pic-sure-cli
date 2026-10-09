package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dialog"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/styles"
	"github.com/JamesPeck/pic-sure-cli/internal/wizard"
)

// The setup wizard screen: the field form, then the confirm-summary. It
// writes nothing; on consent the app runs init with what was entered.
// Calm background, no starfield.
type wizardPhase int

const (
	wizardMain wizardPhase = iota
	wizardConfirm
	// wizardDone is the terminal phase, entered before the screen reports
	// its result, so a huh blink tick arriving before the app acts can't
	// complete the form a second time.
	wizardDone
)

// wizardClosedMsg tells the app the wizard was left without consent, or
// with a setup it couldn't use (err).
type wizardClosedMsg struct{ err error }

// wizardDoneMsg carries the confirmed setup.
type wizardDoneMsg struct {
	doc     *stack.ConfigDoc
	secrets stack.UserSecrets
}

var (
	wizardTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(styles.Brand).Padding(0, 1)
	wizardFooterStyle = lipgloss.NewStyle().Faint(true).Padding(0, 1)
)

type wizardScreen struct {
	wf    *wizard.Form
	phase wizardPhase
	// discarding is set when esc or ctrl+c is pressed on a modified form:
	// the screen asks "Discard setup? (y/n)" before closing. A pristine
	// form closes at once.
	discarding    bool
	width, height int
}

// newWizardScreen opens the form with base's values and sec's secrets;
// defaults are the values its summary marks "(default)".
func newWizardScreen(defaults, base stack.Config, sec stack.UserSecrets) *wizardScreen {
	return &wizardScreen{wf: wizard.NewFormFrom(defaults, base, sec)}
}

func (s *wizardScreen) init() tea.Cmd { return s.wf.Main.Init() }

// setSize sizes the forms on resize, and before init.
func (s *wizardScreen) setSize(width, height int) {
	s.width, s.height = width, height
	s.wf.Main = s.applySize(s.wf.Main)
	if s.wf.Confirm != nil {
		s.wf.Confirm = s.applySize(s.wf.Confirm)
	}
}

// applySize fits the form to the screen (dialog.Fit).
func (s *wizardScreen) applySize(f *huh.Form) *huh.Form {
	height := 40 // unsized yet: don't constrain the content
	if s.height > 0 {
		height = max(s.height-4, 8)
	}
	return dialog.Fit(f, max(min(s.width-4, 76), 40), height)
}

func (s *wizardScreen) update(msg tea.Msg) (*wizardScreen, tea.Cmd) {
	if s.phase == wizardDone {
		return s, nil
	}
	// The discard question owns the keyboard until answered; other
	// messages (huh blink ticks) are dropped so it stays put.
	if s.discarding {
		key, ok := msg.(tea.KeyPressMsg)
		if !ok {
			return s, nil
		}
		switch key.String() {
		case "y", "Y":
			return s, closeWizard
		case "n", "N", "esc", "ctrl+c":
			s.discarding = false
		}
		return s, nil
	}
	// huh ships its esc binding disabled, and its ctrl+c aborts without
	// asking, so the screen handles both.
	if key, ok := msg.(tea.KeyPressMsg); ok && (key.String() == "esc" || key.String() == "ctrl+c") {
		if s.wf.Dirty() {
			s.discarding = true
			return s, nil
		}
		return s, closeWizard
	}

	switch s.phase {
	case wizardMain:
		cmd := s.wf.Update(msg)
		switch s.wf.Main.State {
		case huh.StateAborted:
			return s, closeWizard
		case huh.StateCompleted:
			s.phase = wizardConfirm
			s.wf.Confirm = s.applySize(s.wf.BuildConfirm())
			return s, s.wf.Confirm.Init()
		}
		return s, cmd
	default: // wizardConfirm
		form, cmd := s.wf.Confirm.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			s.wf.Confirm = f
		}
		switch s.wf.Confirm.State {
		case huh.StateAborted:
			return s, closeWizard
		case huh.StateCompleted:
			if !s.wf.Confirmed() {
				return s, closeWizard
			}
			s.phase = wizardDone
			doc, sec, err := s.wf.Result()
			if err != nil {
				return s, func() tea.Msg { return wizardClosedMsg{err: err} }
			}
			return s, func() tea.Msg { return wizardDoneMsg{doc: doc, secrets: sec} }
		}
		return s, cmd
	}
}

func closeWizard() tea.Msg { return wizardClosedMsg{} }

func (s *wizardScreen) view() string {
	body := s.wf.Main.View()
	if s.phase != wizardMain {
		body = s.wf.Confirm.View()
	}
	footer := "esc cancel"
	if s.discarding {
		footer = "Discard setup? (y/n)"
	}
	content := lipgloss.JoinVertical(lipgloss.Left,
		wizardTitleStyle.Render("Set up PIC-SURE"), body, wizardFooterStyle.Render(footer))
	if s.width == 0 || s.height == 0 {
		return content
	}
	return lipgloss.Place(s.width, s.height, lipgloss.Center, lipgloss.Center, content)
}
