package cli

import "github.com/spf13/cobra"

func newSecretsCmd(a *App) *cobra.Command {
	return newGroup("secrets", "Manage the stack's secrets",
		&cobra.Command{
			Use:   "rotate NAME",
			Short: "Rotate a secret everywhere it is used",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("058"),
		},
	)
}
