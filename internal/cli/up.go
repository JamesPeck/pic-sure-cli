package cli

import "github.com/spf13/cobra"

func newUpCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Converge an existing stack to running",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("035"),
	}
}
