package cli

import "github.com/JamesPeck/pic-sure-cli/internal/docker"

// newEngine returns the docker Engine, which runs the docker CLI through r.
func (a *App) newEngine(r docker.Runner) docker.Engine { return docker.NewEngine(r) }
