package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// exampleExempt are the groups that only hold subcommands, so --help on
// them lists the subcommands instead of examples.
var exampleExempt = map[string]bool{
	"pic-sure cache":       true,
	"pic-sure config":      true,
	"pic-sure data":        true,
	"pic-sure db":          true,
	"pic-sure dev":         true,
	"pic-sure dictionary":  true,
	"pic-sure secrets":     true,
	"pic-sure shared-data": true,
}

// exampleCommands returns every command under root that must carry an
// Example: all but root itself, hidden commands, cobra's help and
// completion, and exampleExempt.
func exampleCommands(t *testing.T, root *cobra.Command) []*cobra.Command {
	t.Helper()
	var out []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			if exampleExempt[sub.CommandPath()] {
				if !sub.HasSubCommands() {
					t.Errorf("%s is exempt from examples but has no subcommands", sub.CommandPath())
				}
			} else {
				out = append(out, sub)
			}
			walk(sub)
		}
	}
	walk(root)
	return out
}

func TestEveryCommandHasExamples(t *testing.T) {
	root := newDocRoot()
	root.InitDefaultCompletionCmd()
	cmds := exampleCommands(t, root)
	for path := range exampleExempt {
		if c, _, err := root.Find(strings.Fields(path)[1:]); err != nil || c.CommandPath() != path {
			t.Errorf("exempt group %s doesn't exist", path)
		}
	}
	for _, c := range cmds {
		path := c.CommandPath()
		if strings.TrimSpace(c.Example) == "" {
			t.Errorf("%s has no Example", path)
			continue
		}
		var examples, own int
		pending := ""
		for _, line := range strings.Split(c.Example, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case line == "":
				continue
			case strings.HasPrefix(line, "#"):
				if pending != "" {
					t.Errorf("%s: comment %q has no example below it", path, pending)
				}
				pending = line
				continue
			case pending == "":
				t.Errorf("%s: example %q has no # comment above it", path, line)
			}
			pending = ""
			examples++
			for _, args := range picSureInvocations(line) {
				if err := checkExample(args); err != nil {
					t.Errorf("%s: example %q: %v", path, line, err)
					continue
				}
				if found, _, _ := root.Find(args); found == c {
					own++
				}
			}
		}
		if pending != "" {
			t.Errorf("%s: comment %q has no example below it", path, pending)
		}
		if examples < 2 || examples > 4 {
			t.Errorf("%s has %d examples, want 2 to 4", path, examples)
		}
		if own == 0 {
			t.Errorf("%s: no example runs %s", path, path)
		}
	}
}

// checkExample is checkInvocation without its allowance for prose: an
// example must run as written, so it fails on a missing argument and on
// a missing required or conflicting flag too. It runs nothing, so it
// can't see requirements a command checks only when it runs.
func checkExample(args []string) error {
	cmd, rest, err := newDocRoot().Find(args)
	if err != nil {
		return err
	}
	cmd.InitDefaultHelpFlag()
	if err := cmd.ParseFlags(rest); err != nil {
		return err
	}
	if help, _ := cmd.Flags().GetBool("help"); help {
		return nil
	}
	pos := cmd.Flags().Args()
	if cmd.HasAvailableSubCommands() {
		if len(pos) > 0 {
			return fmt.Errorf("%s has no subcommand %q", cmd.CommandPath(), pos[0])
		}
		if cmd.HasParent() {
			// Root alone opens the TUI; a group alone is an error.
			return fmt.Errorf("%s needs a subcommand", cmd.CommandPath())
		}
	}
	if err := cmd.ValidateArgs(pos); err != nil {
		return err
	}
	if err := cmd.ValidateRequiredFlags(); err != nil {
		return err
	}
	return cmd.ValidateFlagGroups()
}
