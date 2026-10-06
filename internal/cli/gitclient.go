package cli

import (
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// newGitClient returns the git Client. Ticket 018 replaces the stub.
func (a *App) newGitClient(docker.Runner) git.Client { return nil }
