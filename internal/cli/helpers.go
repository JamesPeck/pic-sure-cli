package cli

import (
	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// notImplemented is the RunE of a command whose ticket hasn't landed.
func notImplemented(ticket string) func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error {
		return exitcode.Failed("not implemented (ticket %s)", ticket)
	}
}

// newGroup returns a parent command for subs. Without a subcommand, or with
// an unknown one, it fails with a usage error.
func newGroup(use, short string, subs ...*cobra.Command) *cobra.Command {
	g := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitcode.Usage("%s needs a subcommand", cmd.CommandPath())
		},
	}
	g.AddCommand(subs...)
	return g
}
