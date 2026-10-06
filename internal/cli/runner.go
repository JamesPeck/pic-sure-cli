package cli

import (
	"context"
	"io"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// newRunner returns the process runner. Ticket 003 replaces the stub with
// the exec runner.
func (a *App) newRunner() docker.Runner { return stubRunner{} }

type stubRunner struct{}

func (stubRunner) Run(context.Context, docker.Cmd) (docker.Result, error) {
	return docker.Result{}, errRunnerStub
}

func (stubRunner) Stream(context.Context, docker.Cmd, io.Writer, io.Writer) (int, error) {
	return 0, errRunnerStub
}

var errRunnerStub = exitcode.Failed("running commands: not implemented (ticket 003)")
