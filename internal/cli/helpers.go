package cli

import (
	"strings"

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

// newHelpCmd replaces cobra's help command, which prints the root help and
// exits 0 for an unknown topic.
func newHelpCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(_ *cobra.Command, args []string) error {
			target, rest, err := root.Find(args)
			if err != nil || len(rest) > 0 {
				return exitcode.Usage("unknown help topic %q", strings.Join(args, " "))
			}
			return target.Help()
		},
	}
}
