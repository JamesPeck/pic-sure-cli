package ops

import (
	"io"
	"log/slog"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
)

// Deps is everything an operation uses to touch the outside world. The cli
// layer builds one per command run; tests build one from fakes, so an
// operation never reaches for a global (time.Now, crypto/rand, os.Stdout,
// exec.Command) directly.
type Deps struct {
	// Runner runs subprocesses. Prefer Docker, Compose and Git, which are
	// built on it; use Runner directly only for a program none of them
	// wraps.
	Runner docker.Runner
	// Docker drives the docker CLI (ticket 016).
	Docker docker.Engine
	// Compose drives `docker compose` for the stack the command acts on
	// (ticket 017). It is nil until the command has found or created a
	// rendered stack.
	Compose docker.Composer
	// Git drives the user's git (ticket 018).
	Git git.Client
	// Clock is the time source.
	Clock Clock
	// Rand is the randomness source for secrets: crypto/rand.Reader in
	// production, a seeded reader in tests.
	Rand io.Reader
	// Sink receives the operation's events.
	Sink events.Sink
	// Log is the debug logger (ticket 005). Event text is for the user;
	// Log is for bug reports.
	Log *slog.Logger
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// FixedClock is a Clock stopped at one instant, for tests.
type FixedClock time.Time

func (c FixedClock) Now() time.Time { return time.Time(c) }
