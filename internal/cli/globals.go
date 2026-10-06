package cli

import "github.com/spf13/cobra"

// GlobalOptions are the flags every command accepts (spec §5). They are
// persistent flags on the root, so they go anywhere on the command line.
type GlobalOptions struct {
	// Stack is --stack DIR: the stack to act on. Empty means the stack
	// containing the current directory (see stack.go).
	Stack string
	// WaitLock is --wait-lock: when another command holds the stack lock,
	// wait for it instead of failing.
	WaitLock bool
	// JSON is --json: NDJSON events on stdout, or one JSON object for
	// read-only reports (ticket 004). It implies NonInteractive.
	JSON bool
	// Plain is --plain: timestamped plain output instead of the TUI.
	Plain bool
	// Yes is --yes: answer yes to every confirmation, destructive ones
	// included, and never prompt. It never implies --self-update (D12).
	Yes bool
	// NonInteractive is --non-interactive: never prompt. A question that
	// needs an answer is an error instead, and a destructive command still
	// needs --yes (exit 4 without it).
	NonInteractive bool
	// NoAnimations is --no-animations: a static TUI.
	NoAnimations bool
	// LogLevel is --log-level: the stderr log level (ticket 005).
	LogLevel string
	// SkipSteps is --skip-step ID, repeatable: steps a converging command
	// must skip (ticket 011).
	SkipSteps []string
}

func (g *GlobalOptions) register(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.StringVar(&g.Stack, "stack", "", "act on the stack in `DIR` (default: the stack containing the current directory)")
	f.BoolVar(&g.WaitLock, "wait-lock", false, "if another pic-sure command is changing the stack, wait for it instead of failing")
	f.BoolVar(&g.JSON, "json", false, "machine-readable JSON on stdout; implies --non-interactive")
	f.BoolVar(&g.Plain, "plain", false, "plain timestamped output instead of the TUI")
	f.BoolVar(&g.Yes, "yes", false, "answer yes to every confirmation, including destructive ones")
	f.BoolVar(&g.NonInteractive, "non-interactive", false, "never prompt; fail when an answer is needed")
	f.BoolVar(&g.NoAnimations, "no-animations", false, "static TUI, without animation")
	f.StringVar(&g.LogLevel, "log-level", "info", "stderr log `LEVEL`: debug, info, warn or error")
	f.StringArrayVar(&g.SkipSteps, "skip-step", nil, "skip the step with this `ID` (repeatable; converging commands only)")
	cmd.MarkFlagsMutuallyExclusive("json", "plain")
}
