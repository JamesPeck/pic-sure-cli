// Package dialog holds the huh dialog helpers shared by the TUI's screens
// (internal/tui) and the dashboard (internal/dashboard): Fit, which sizes
// and styles any embedded form, and the confirmations for actions.
// internal/tui imports internal/dashboard, so the dashboard can't reach
// back into tui for them.
package dialog

import (
	"fmt"

	"charm.land/huh/v2"
)

// TeardownForm builds the typed confirmation for reset or destroy of the
// stack name in root: the user types the name, as `pic-sure reset` and
// `destroy` ask on a terminal. Reset also asks whether to keep the
// database, into keepDB. The form is returned unfitted.
func TeardownForm(name, root string, destroy bool, keepDB *bool, typed *string) *huh.Form {
	input := huh.NewInput().
		Title(fmt.Sprintf("Type the stack's name, %s, to confirm", name)).
		Value(typed).
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
				Title("⚠ Destroy: this deletes the stack").
				Description("Stops and removes the containers and every volume of this stack,\n" +
					"the database included, and then the files pic-sure created in\n" +
					fmt.Sprintf("%s. Files you added there are kept.", root)),
			input,
		}
	} else {
		fields = []huh.Field{
			huh.NewSelect[bool]().
				Title("⚠ Reset: this deletes data").
				Description("Stops the stack and removes its data volumes and TLS certificate.\n"+
					"The config, secrets and logs are kept; pic-sure up sets it up again.").
				Value(keepDB).
				Options(
					huh.NewOption("Remove the database too", false),
					huh.NewOption("Keep the database", true),
				),
			input,
		}
	}
	return huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(true)
}

// ConfirmForm builds the yes/no dialog for a safe action, into ok. The form
// is returned unfitted.
func ConfirmForm(question, describe string, ok *bool) *huh.Form {
	return huh.NewForm(huh.NewGroup(huh.NewConfirm().
		Title(question).
		Description(describe).
		Affirmative("Run").
		Negative("Cancel").
		Value(ok))).WithShowHelp(true)
}
