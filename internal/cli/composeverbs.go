package cli

import "github.com/spf13/cobra"

// The thin compose verbs (ticket 026).

func newDownCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the stack's containers (volumes are kept)",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("026"),
	}
}

func newRestartCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "restart [SERVICE...]",
		Short: "Restart services (default: all)",
		RunE:  notImplemented("026"),
	}
}

func newPsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "List the stack's containers",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("026"),
	}
}

func newLogsCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "logs [SERVICE]",
		Short: "Show service logs",
		Args:  cobra.MaximumNArgs(1),
		RunE:  notImplemented("026"),
	}
	c.Flags().BoolP("follow", "f", false, "follow log output")
	return c
}

func newComposeCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "compose -- ARGS...",
		Short: "Run docker compose against the rendered stack (escape hatch)",
		Long: `Run docker compose against the rendered stack, with the same -f files and
environment the CLI uses. Put -- before the compose arguments.`,
		RunE: notImplemented("026"),
	}
}
