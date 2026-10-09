package cli

import (
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

// dockerPrecondition turns a failure (exit 1) caused by a missing docker
// executable or compose plugin, or an unreachable daemon, into exit 3
// (spec §10.4), with the advice init's preflight gives. Any other exit
// code stays. doctor's own failure ("N check(s) failed") quotes none of
// docker's errors, so it stays exit 1.
func dockerPrecondition(err error) error {
	if exitcode.FromError(err) != exitcode.CodeFailed {
		return err
	}
	switch {
	case docker.IsMissing(err):
		return exitcode.Precondition("docker is not on PATH: %s (%w)", docker.InstallHint, err)
	case docker.IsUnreachable(err):
		return exitcode.Precondition("the Docker daemon isn't reachable: %w\n%s", err, docker.StartHint)
	case docker.IsComposeMissing(err):
		return exitcode.Precondition("docker compose isn't installed; pic-sure needs %s or later (%w)", ops.MinComposeVersion, err)
	}
	return err
}
