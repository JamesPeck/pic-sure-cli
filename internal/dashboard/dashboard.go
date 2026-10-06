// Package dashboard is the pic-sure dashboard screen: live service/status
// panes, a log follower, and keybound actions with an output pane. It is
// embedded as a sibling screen of the unified TUI (internal/tui).
//
// v1 read everything through the AIO scripts and ran actions as scripts in a
// PTY. That layer is gone in v2, so the polls, the log follower and every
// action report "not implemented" until ticket 040 rewires them onto the v2
// operations.
package dashboard

import (
	tea "github.com/charmbracelet/bubbletea"
)

// BackMsg asks the embedding program to leave the dashboard (esc in normal
// mode).
type BackMsg struct{}

// New builds the dashboard model for embedding by the unified TUI.
func New(root string) tea.Model {
	return newModel(root)
}
