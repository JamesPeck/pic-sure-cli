package cli

import (
	"crypto/rand"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
)

// newDeps assembles the dependencies for one command run. Each comes from a
// constructor in its owning ticket's wiring file (see the package doc), so
// wiring a new implementation never touches this function. Deps.Compose is
// left nil: it belongs to one rendered stack, so the command sets it once it
// has found or created the stack (tickets 007, 017, 021).
func (a *App) newDeps() *ops.Deps {
	runner := a.newRunner()
	return &ops.Deps{
		Runner: runner,
		Docker: a.newEngine(runner),
		Git:    a.newGitClient(runner),
		Clock:  ops.SystemClock{},
		Rand:   rand.Reader,
		Sink:   a.newSink(),
		Log:    a.newLogger(),
	}
}
