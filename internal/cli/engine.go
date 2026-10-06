package cli

import "github.com/JamesPeck/pic-sure-cli/internal/docker"

// newEngine returns the docker Engine. Ticket 016 replaces the stub.
func (a *App) newEngine(docker.Runner) docker.Engine { return nil }
