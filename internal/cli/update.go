package cli

import "github.com/spf13/cobra"

func newUpdateCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "update",
		Short: "Update the stack to the current release: config, images, migrations",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("036"),
	}
	f := c.Flags()
	f.Bool("dry-run", false, "print the plan and change nothing")
	f.String("release-commit", "", "use this release-control `SHA` instead of the branch head")
	f.Bool("no-build", false, "skip building and pulling images")
	f.Bool("self-update", false, "replace this binary if the release names a newer CLI")
	f.Bool("ignore-cli-version", false, "skip the CLI compatibility gate")
	return c
}
