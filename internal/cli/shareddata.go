package cli

import "github.com/spf13/cobra"

func newSharedDataCmd(a *App) *cobra.Command {
	return newGroup("shared-data", "Publish HPDS data for other stacks to mount read-only",
		&cobra.Command{
			Use:   "publish NAME",
			Short: "Publish this stack's HPDS data as an immutable data set",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("050"),
		},
		&cobra.Command{
			Use:   "list",
			Short: "List the published data sets",
			Args:  cobra.NoArgs,
			RunE:  notImplemented("050"),
		},
		&cobra.Command{
			Use:   "remove NAME",
			Short: "Remove a data set no container uses",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("050"),
		},
	)
}
