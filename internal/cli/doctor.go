package cli

import "github.com/spf13/cobra"

func newDoctorCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check the host, Docker, the stack and the network",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("025"),
	}
	c.Flags().Bool("network", false, "also check reachability through the proxy")
	return c
}
