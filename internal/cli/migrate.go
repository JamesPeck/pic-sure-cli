package cli

import "github.com/spf13/cobra"

func newMigrateCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Run the database migrations",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("032"),
	}
	c.Flags().Bool("check", false, "verify the migration inputs without running them")
	c.Flags().Bool("repair", false, "run Flyway repair")
	c.MarkFlagsMutuallyExclusive("check", "repair")
	return c
}
