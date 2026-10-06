package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionString(a.Info))
			return err
		},
	}
}

// versionString names the product line ("v2 (native)", which tells it apart
// from the v1 script wrapper of the same name; spec §14) and the build.
func versionString(info BuildInfo) string {
	return fmt.Sprintf("pic-sure v2 (native)\nversion %s, commit %s, built %s", info.Version, info.Commit, info.Date)
}
