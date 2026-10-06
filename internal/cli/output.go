package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// newSink returns the event sink for the output mode. Ticket 004 replaces
// the stub with the plain, NDJSON and (via 038) TUI renderers.
func (a *App) newSink() events.Sink { return events.Discard }

// reportError tells the user why cmd failed. Ticket 004 adds the --json
// form, an events.Result carrying the exit code and message.
func (a *App) reportError(cmd *cobra.Command, err error) {
	_, _ = fmt.Fprintf(a.Stderr, "pic-sure: %v\n", err)
	if exitcode.FromError(err) == exitcode.CodeUsage && cmd != nil {
		_, _ = fmt.Fprintf(a.Stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
	}
}
