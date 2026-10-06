package cli

import (
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// newGitClient returns the git Client. A command that has loaded its stack
// adds the proxy variables with WithEnv.
func (a *App) newGitClient(r docker.Runner) git.Client { return git.New(r) }
