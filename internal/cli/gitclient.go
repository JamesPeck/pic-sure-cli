package cli

import (
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// Commands add the stack's proxy variables to this client with WithEnv once
// they have loaded the stack.
func (a *App) newGitClient(r docker.Runner) git.Client { return git.New(r) }
