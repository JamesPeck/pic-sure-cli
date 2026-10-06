package cli

import "log/slog"

// newLogger returns the debug logger. Ticket 005 replaces the stub with the
// stderr handler for --log-level, the per-run log file and redaction.
func (a *App) newLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
