package cli

import "github.com/spf13/cobra"

func newBuildCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "build [COMPONENT...]",
		Short: "Build the stack's images (default: every component)",
		RunE:  notImplemented("031"),
	}
	c.Flags().Bool("force", false, "rebuild even if the images exist")
	return c
}
