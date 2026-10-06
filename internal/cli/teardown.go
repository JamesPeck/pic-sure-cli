package cli

import "github.com/spf13/cobra"

// reset and destroy (ticket 056).

func newResetCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "reset",
		Short: "Remove the stack's containers and data volumes; keep its config",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("056"),
	}
	c.Flags().Bool("keep-db", false, "keep the database volume")
	return c
}

func newDestroyCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "destroy",
		Short: "Remove everything the CLI created for this stack",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("056"),
	}
	c.Flags().Bool("prune-images", false, "also remove shared images no other stack uses")
	return c
}
