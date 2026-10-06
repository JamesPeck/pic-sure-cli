package actions

import "fmt"

// MaxOutput caps retained action scrollback (1 MiB), shared by all surfaces.
const MaxOutput = 1 << 20

// OutputMsg carries a batch of action output bytes.
type OutputMsg struct{ Data []byte }

// DoneMsg reports an action's exit. Err is non-nil only for start/wait
// failures; Code is the exit status (128+N for signal deaths).
type DoneMsg struct {
	Code int
	Err  error
}

// NotImplemented is the error every action start returns until the TUI runs
// v2 operations in-process.
func NotImplemented(act Action) error {
	return fmt.Errorf("%s: not implemented in v2 yet (ticket %s)", act.Name, act.Ticket)
}
