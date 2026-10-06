package cli

import "github.com/spf13/cobra"

func newCacheCmd(a *App) *cobra.Command {
	return newGroup("cache", "Inspect and prune the host cache",
		&cobra.Command{
			Use:   "list",
			Short: "List cached sources, images and downloads, and what uses them",
			Args:  cobra.NoArgs,
			RunE:  notImplemented("057"),
		},
		&cobra.Command{
			Use:   "prune",
			Short: "Remove cache entries no stack uses",
			Args:  cobra.NoArgs,
			RunE:  notImplemented("057"),
		},
	)
}
