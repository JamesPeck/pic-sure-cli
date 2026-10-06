package cli

import "github.com/spf13/cobra"

func newSupportBundleCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "support-bundle",
		Short: "Write a redacted diagnostics archive to attach to an issue",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("059"),
	}
	c.Flags().StringP("output", "o", "", "write the archive to `FILE`")
	return c
}
