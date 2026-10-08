package dashboard

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dialog"
)

// Confirmation is a yes/no dialog's question and description.
type Confirmation struct{ Question, Describe string }

func restartAction(service string) (Action, Confirmation) {
	return Action{
			Title: "Restarting " + service,
			Done:  "Restarted " + service,
			Args:  []string{"restart", service},
		}, Confirmation{"Restart " + service + "?",
			fmt.Sprintf("Restarts the %s container.", service)}
}

// UpdateAction is `update`, which the dashboard and the landing both offer.
func UpdateAction() (Action, Confirmation) {
	return Action{
			Title: "Updating PIC-SURE",
			Done:  "Update finished",
			Args:  []string{"update"},
		}, Confirmation{"Update PIC-SURE?",
			"Fetches the release, rebuilds what changed, runs the migrations,\n" +
				"renews the introspection token and restarts what needs it.\n" +
				"Data volumes are kept."}
}

// MigrateAction is `migrate`, which the dashboard and the landing both offer.
func MigrateAction() (Action, Confirmation) {
	return Action{
			Title: "Migrating the databases",
			Done:  "Migrations applied",
			Args:  []string{"migrate"},
		}, Confirmation{"Run the database migrations?",
			"Runs the pending Flyway migrations on the PIC-SURE and dictionary\ndatabases."}
}

// TeardownAction is `reset`, keeping the database with keepDB, or with
// destroy `destroy`. It carries --yes: the user has typed the stack's name
// (dialog.TeardownForm).
func TeardownAction(destroy, keepDB bool) Action {
	if destroy {
		return Action{Title: "Destroying the stack", Done: "Stack destroyed", Args: []string{"--yes", "destroy"}}
	}
	a := Action{Title: "Resetting the stack", Done: "Stack reset", Args: []string{"--yes", "reset"}}
	if keepDB {
		a.Args = append(a.Args, "--keep-db")
	}
	return a
}

func (m *model) startConfirm(act Action, c Confirmation) (tea.Model, tea.Cmd) {
	m.pending = &act
	m.confirmOK = false
	m.form = m.sizeForm(huh.NewForm(huh.NewGroup(huh.NewConfirm().
		Title(c.Question).
		Description(c.Describe).
		Affirmative("Run").
		Negative("Cancel").
		Value(&m.confirmOK))).WithShowHelp(true))
	return m, m.form.Init()
}

// startTeardown opens the typed confirmation for reset or destroy.
func (m *model) startTeardown(destroy bool) (tea.Model, tea.Cmd) {
	name := m.stackName()
	if name == "" {
		m.lastResult = "the stack's name isn't known yet; wait for the status to load"
		return m, nil
	}
	m.teardownName = name
	m.teardownDestroy = destroy
	m.keepDB = false
	m.confirmText = ""
	m.form = m.sizeForm(dialog.TeardownForm(name, m.root, destroy, &m.keepDB, &m.confirmText))
	return m, m.form.Init()
}

// sizeForm fits a dialog form (dialog.Fit) to the FORM PANE it renders in
// (m.width-leftWidth()-8), not the whole terminal. Using WithWidth, or sizing
// to the terminal, lays the form out wider than the pane so lipgloss re-wraps
// every line inside it, mangling titles/descriptions below 120 cols.
func (m *model) sizeForm(f *huh.Form) *huh.Form {
	cols := m.formWidth()
	// -5 = the frame's chrome rows around the form pane content: header (1) +
	// pane border top/bottom (2) + footer help line (1), plus 1 row of slack
	// so the composed frame can never exceed the terminal box.
	height := max(m.height-5, 8)
	return dialog.Fit(f, cols, height)
}

func (m *model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	form, cmd := m.form.Update(msg)
	if f, ok := form.(*huh.Form); ok {
		m.form = f
	}

	switch m.form.State {
	case huh.StateCompleted:
		m.form = nil
		switch {
		case m.pending != nil:
			act := *m.pending
			m.pending = nil
			if !m.confirmOK {
				return m, nil
			}
			return m, run(act)
		case m.confirmText != m.teardownName:
			// The form's own validation gates real input.
			return m, nil
		default:
			return m, run(TeardownAction(m.teardownDestroy, m.keepDB))
		}
	case huh.StateAborted:
		m.closeForm()
		return m, nil
	}
	return m, cmd
}

func (m *model) closeForm() {
	m.form = nil
	m.pending = nil
}

func run(act Action) tea.Cmd {
	return func() tea.Msg { return RunMsg{Action: act} }
}
