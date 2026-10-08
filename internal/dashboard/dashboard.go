// Package dashboard is the pic-sure dashboard screen: the services compose
// reports, the stack's status, an on-demand deep health check, the selected
// service's logs, and keybound actions. It is embedded as a screen of the
// unified TUI (internal/tui), which runs the actions it asks for.
package dashboard

import (
	"context"
	"io"

	tea "charm.land/bubbletea/v2"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

// Backend is how the dashboard reads the stack. Every method may be called
// from any goroutine, and must return once ctx is done.
type Backend interface {
	// Services is `compose ps`.
	Services(ctx context.Context) ([]ops.StatusService, error)
	// Status is `status`, or `status --deep` with deep.
	Status(ctx context.Context, deep bool) (*ops.StatusReport, error)
	// FollowLogs writes service's last lines and then its new ones to w,
	// as `logs -f` does, until ctx is done or compose stops.
	FollowLogs(ctx context.Context, service string, w io.Writer) error
}

// Action is a command the dashboard asks the embedding program to run.
type Action struct {
	// Title heads the screen that runs it, such as "Updating PIC-SURE".
	Title string
	// Done is the line shown when it succeeds.
	Done string
	// Args is the pic-sure command line, without the global --stack: for
	// example ["restart", "hpds"]. A destructive one carries --yes, given
	// once the user typed the stack's name.
	Args []string
	// NoStack runs it without --stack, for the landing's preflight check
	// in a directory that holds no stack yet: doctor then checks only the
	// host and Docker.
	NoStack bool
}

// RunMsg asks the embedding program to run an action. It sends ActionDoneMsg
// back to the dashboard once the user is done with the action's screen.
type RunMsg struct{ Action Action }

// ActionDoneMsg tells the dashboard its action's screen has closed. The
// dashboard drops its cached deep check, polls again and follows the logs
// again.
type ActionDoneMsg struct{}

// LoadMsg asks the embedding program to open its load wizard (l). A load
// the wizard runs is followed by ActionDoneMsg, as an action is.
type LoadMsg struct{}

// BackMsg asks the embedding program to leave the dashboard (esc).
type BackMsg struct{}

// New builds the dashboard for the stack in root. ctx bounds its polls and
// log follower; leaving the dashboard stops them too.
func New(ctx context.Context, root string, b Backend) tea.Model {
	return newModel(ctx, root, b)
}

// Owns reports whether msg is one of the dashboard's own messages: its
// polls' ticks and results and its log lines. The embedding program must
// route them to the dashboard even while another screen is showing, or the
// polls and the log follower stop.
func Owns(msg tea.Msg) bool {
	_, ok := msg.(ownMsg)
	return ok
}
