package cli

import "github.com/spf13/cobra"

func newSelfUpdateCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "self-update",
		Short: "Replace this binary with a verified release",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("060"),
	}
	c.Flags().String("to", "", "install this `VERSION` instead of the latest")
	return c
}
