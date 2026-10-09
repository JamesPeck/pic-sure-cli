package cli

import (
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// dockerPrecondition turns a failure (exit 1) caused by a missing docker
// executable or an unreachable daemon into exit 3 (spec §10.4), with the
// advice init's preflight gives. Any other exit code stays: doctor's
// failed checks have no Docker error in their chain, and status reports
// Docker problems without failing.
func dockerPrecondition(err error) error {
	if exitcode.FromError(err) != exitcode.CodeFailed {
		return err
	}
	switch {
	case docker.IsMissing(err):
		return exitcode.Precondition("docker is not on PATH: %s (%w)", docker.InstallHint, err)
	case docker.IsUnreachable(err):
		return exitcode.Precondition("the Docker daemon isn't reachable: %w\n%s", err, docker.StartHint)
	}
	return err
}
