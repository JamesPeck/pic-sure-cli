package cli

import "github.com/spf13/cobra"

func newDBCmd(a *App) *cobra.Command {
	bootstrap := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create the databases, users and grants on a remote MySQL",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("054"),
	}
	bootstrap.Flags().Bool("check", false, "report schemas, users and grants without changing anything")
	bootstrap.Flags().Bool("sync-passwords", false, "set the users' passwords to match secrets.yaml")
	return newGroup("db", "Manage a remote MySQL (db.mode remote only)", bootstrap)
}
