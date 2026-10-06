package cli

import "github.com/spf13/cobra"

func newConfigCmd(a *App) *cobra.Command {
	return newGroup("config", "Show and change pic-sure.yaml",
		&cobra.Command{
			Use:   "show",
			Short: "Print the stack's config",
			Args:  cobra.NoArgs,
			RunE:  notImplemented("006"),
		},
		&cobra.Command{
			Use:   "get KEY",
			Short: "Print one config value",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("006"),
		},
		&cobra.Command{
			Use:   "set KEY VALUE",
			Short: "Validate and set one config value",
			Args:  cobra.ExactArgs(2),
			RunE:  notImplemented("006"),
		},
		&cobra.Command{
			Use:   "edit",
			Short: "Edit the config in $EDITOR, validating on save",
			Args:  cobra.NoArgs,
			RunE:  notImplemented("006"),
		},
	)
}
