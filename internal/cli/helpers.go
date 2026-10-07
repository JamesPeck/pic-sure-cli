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
			return withUsageHint(exitcode.Usage("%s needs a subcommand", cmd.CommandPath()))
		},
	}
	g.AddCommand(subs...)
	return g
}

// rejectUnknownHelpTopics makes `help` with an unknown topic a usage error.
// cobra's help command prints the root help and exits 0 for one; the rest of
// it (flag listing, completion) is kept.
func rejectUnknownHelpTopics(root *cobra.Command) {
	root.InitDefaultHelpCmd()
	help, _, err := root.Find([]string{"help"})
	if err != nil || help == root || help.Run == nil {
		return
	}
	show := help.Run
	help.Run = nil
	help.RunE = func(cmd *cobra.Command, args []string) error {
		if _, rest, err := root.Find(args); err != nil || len(rest) > 0 {
			return exitcode.Usage("unknown help topic %q", strings.Join(args, " "))
		}
		show(cmd, args)
		return nil
	}
}
