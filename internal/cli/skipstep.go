package cli

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// skipStepAnnotation marks a command that honours --skip-step. The root
// refuses the flag on every other command (refuseSkipStep).
const skipStepAnnotation = "pic-sure/skip-step"

// skippable marks c as honouring --skip-step, and returns it.
func skippable(c *cobra.Command) *cobra.Command {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[skipStepAnnotation] = "true"
	return c
}

// refuseSkipStep is the root's PersistentPreRunE: --skip-step on a command
// that isn't skippable is exit 2, before the command does anything.
func refuseSkipStep(cmd *cobra.Command, _ []string) error {
	if !cmd.Flags().Changed("skip-step") || cmd.Annotations[skipStepAnnotation] != "" {
		return nil
	}
	return exitcode.Usage("--skip-step: %s has no steps to skip; only %s take it",
		commandName(cmd), strings.Join(skipStepCommands(cmd.Root()), ", "))
}

// skipStepCommands are the names of the visible skippable commands under
// root, in help order.
func skipStepCommands(root *cobra.Command) []string {
	var names []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden {
			return
		}
		if c.Annotations[skipStepAnnotation] != "" {
			names = append(names, commandName(c))
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return names
}

// openStackToCheckSkips is openStack for a command that checks --skip-step
// against the stack's config. With --skip-step, it leaves the run log to
// checkStackSkipSteps, so a mistyped ID writes nothing to the stack.
func (a *App) openStackToCheckSkips(cmd *cobra.Command) (*stack.Stack, error) {
	if len(a.Global.SkipSteps) == 0 {
		return a.openStack(cmd)
	}
	return a.openStackUnlogged(cmd)
}

// checkStackSkipSteps is checkSkipSteps for a stack opened by
// openStackToCheckSkips: once the IDs pass, it starts the run log.
func (a *App) checkStackSkipSteps(cmd *cobra.Command, st *stack.Stack, ids []string) error {
	if err := checkSkipSteps(cmd, ids, a.Global.SkipSteps); err != nil {
		return err
	}
	if len(a.Global.SkipSteps) > 0 {
		a.openRunLog(st)
	}
	return nil
}

// commandName is cmd's path without the root's name, or the root's name.
func commandName(cmd *cobra.Command) string {
	if !cmd.HasParent() {
		return cmd.Name()
	}
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

// checkSkipSteps refuses a --skip-step that names none of ids, the steps
// of cmd's plan. Commands call it before any side effect: the lock,
// state.json or the stack registry.
func checkSkipSteps(cmd *cobra.Command, ids, skips []string) error {
	for _, id := range skips {
		if !slices.Contains(ids, id) {
			return exitcode.Usage("--skip-step %s: %s has no such step; it can skip %s", id, commandName(cmd), strings.Join(ids, ", "))
		}
	}
	return nil
}
