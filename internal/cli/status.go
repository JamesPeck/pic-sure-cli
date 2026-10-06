package cli

import "github.com/spf13/cobra"

func newStatusCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Report the stack's config, versions, images, services and migrations",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("027"),
	}
	c.Flags().Bool("deep", false, "also probe inside the containers: gateway, HPDS data, frontend CSP (ticket 037)")
	return c
}
