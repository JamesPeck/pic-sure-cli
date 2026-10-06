package cli

import "github.com/spf13/cobra"

func newDevCmd(a *App) *cobra.Command {
	return newGroup("dev", "Run services from local source with debug ports",
		&cobra.Command{
			Use:   "list",
			Short: "List the services that have a dev mode",
			Args:  cobra.NoArgs,
			RunE:  notImplemented("052"),
		},
		&cobra.Command{
			Use:   "on SERVICE",
			Short: "Run SERVICE from local source",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("052"),
		},
		&cobra.Command{
			Use:   "off SERVICE",
			Short: "Return SERVICE to its release image",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("052"),
		},
	)
}
