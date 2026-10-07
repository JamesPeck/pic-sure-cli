package cli

import (
	"log/slog"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// newRunner returns the process runner, which logs each command's argv to
// log at debug level.
func (a *App) newRunner(log *slog.Logger) docker.Runner {
	return &docker.ExecRunner{Log: log}
}

// newForegroundRunner returns the runner for an interactive command, whose
// child reads the terminal (docker.ExecRunner.Foreground).
func (a *App) newForegroundRunner(log *slog.Logger) docker.Runner {
	return &docker.ExecRunner{Log: log, Foreground: true}
}
