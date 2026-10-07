package dashboard

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/dialog"
)

// confirmation is a yes/no dialog's question and description.
type confirmation struct{ question, describe string }

func restartAction(service string) (Action, confirmation) {
	return Action{
			Title: "Restarting " + service,
			Done:  "Restarted " + service,
			Args:  []string{"restart", service},
		}, confirmation{"Restart " + service + "?",
			fmt.Sprintf("Restarts the %s container.", service)}
}

func updateAction() (Action, confirmation) {
	return Action{
			Title: "Updating PIC-SURE",
			Done:  "Update finished",
			Args:  []string{"update"},
		}, confirmation{"Update PIC-SURE?",
			"Fetches the release, rebuilds what changed, runs the migrations,\n" +
				"renews the introspection token and restarts what needs it.\n" +
				"Data volumes are kept."}
}

func migrateAction() (Action, confirmation) {
	return Action{
			Title: "Migrating the databases",
			Done:  "Migrations applied",
			Args:  []string{"migrate"},
		}, confirmation{"Run the database migrations?",
			"Runs the pending Flyway migrations on the PIC-SURE and dictionary\ndatabases."}
}

func resetAction(keepDB bool) Action {
	a := Action{Title: "Resetting the stack", Done: "Stack reset", Args: []string{"--yes", "reset"}}
	if keepDB {
		a.Args = append(a.Args, "--keep-db")
	}
	return a
}

func destroyAction() Action {
	return Action{Title: "Destroying the stack", Done: "Stack destroyed", Args: []string{"--yes", "destroy"}}
}

func (m *model) startConfirm(act Action, c confirmation) (tea.Model, tea.Cmd) {
	m.pending = &act
	m.confirmOK = false
	m.form = m.sizeForm(huh.NewForm(huh.NewGroup(huh.NewConfirm().
		Title(c.question).
		Description(c.describe).
		Affirmative("Run").
		Negative("Cancel").
		Value(&m.confirmOK))).WithShowHelp(true))
	return m, m.form.Init()
}

// startTeardown opens the typed confirmation for reset or destroy: the user
// types the stack's name, as `pic-sure reset` and `destroy` ask on a terminal.
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
	typed := huh.NewInput().
		Title(fmt.Sprintf("Type the stack's name, %s, to confirm", name)).
		Value(&m.confirmText).
		Validate(func(s string) error {
			if s != name {
				return fmt.Errorf("type %q exactly to confirm", name)
			}
			return nil
		})
	var fields []huh.Field
	if destroy {
		fields = []huh.Field{
			huh.NewNote().
				Title("⚠ Destroy — this deletes the stack").
				Description("Stops and removes the containers and every volume of this stack,\n" +
					"the database included, and then the files pic-sure created in\n" +
					fmt.Sprintf("%s. Files you added there are kept.", m.root)),
			typed,
		}
	} else {
		fields = []huh.Field{
			huh.NewSelect[bool]().
				Title("⚠ Reset — this deletes data").
				Description("Stops the stack and removes its data volumes and TLS certificate.\n"+
					"The config, secrets and logs are kept; pic-sure up sets it up again.").
				Value(&m.keepDB).
				Options(
					huh.NewOption("Remove the database too", false),
					huh.NewOption("Keep the database", true),
				),
			typed,
		}
	}
	m.form = m.sizeForm(huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(true))
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
		case m.teardownDestroy:
			return m, run(destroyAction())
		default:
			return m, run(resetAction(m.keepDB))
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
