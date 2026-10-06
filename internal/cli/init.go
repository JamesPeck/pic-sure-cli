package cli

import "github.com/spf13/cobra"

func newInitCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "init [DIR]",
		Short: "Create a stack in DIR (default: the current directory) and bring it up",
		Args:  cobra.MaximumNArgs(1),
		RunE:  notImplemented("034"),
	}
}
