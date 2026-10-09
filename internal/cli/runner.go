package cli

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
)

// newRunner returns the process runner, which logs each command's argv to
// log at debug level.
func (a *App) newRunner(log *slog.Logger) docker.Runner {
	return &docker.ExecRunner{Log: log}
}

// newForegroundRunner returns the runner for `compose -- args`, whose
// child reads the terminal (docker.ExecRunner.Foreground). It logs args
// only as logArgs does, since they may hold a password.
func (a *App) newForegroundRunner(log *slog.Logger, args []string) docker.Runner {
	return &docker.ExecRunner{Log: log, Foreground: true, LogArgv: func(argv []string) string {
		if len(argv) < len(args) || !slices.Equal(argv[len(argv)-len(args):], args) {
			return fmt.Sprintf("%s (%d arguments)", argv[0], len(argv)-1)
		}
		own := docker.FormatArgv(argv[:len(argv)-len(args)])
		if i := composeSubcommand(args); i >= 0 {
			return fmt.Sprintf("%s %s (%d more arguments)", own, args[i], len(args)-i-1)
		}
		return fmt.Sprintf("%s (%d more arguments)", own, len(args))
	}}
}
