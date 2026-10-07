package cli

import (
	"errors"
	"io"
	"sync/atomic"
	"syscall"
)

// pipeWriter is the CLI's own stdout or stderr. The first write that fails
// with EPIPE (the reader went away, as with `| head`) calls broken, which
// cancels the run as SIGPIPE would, and every later write fails at once
// without reaching w. main asks for SIGPIPE so such a write returns EPIPE
// instead of killing the process.
//
// A subprocess that should own the terminal gets the raw *os.File instead,
// so it inherits the descriptor.
type pipeWriter struct {
	w      io.Writer
	broken func()
	closed atomic.Bool
}

func (p *pipeWriter) Write(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, syscall.EPIPE
	}
	n, err := p.w.Write(b)
	if errors.Is(err, syscall.EPIPE) && p.closed.CompareAndSwap(false, true) {
		p.broken()
	}
	return n, err
}

// stdout is the run's stdout for what the CLI writes itself.
func (a *App) stdout() io.Writer {
	if a.outW == nil {
		return a.Stdout
	}
	return a.outW
}

// stderr is the run's stderr for what the CLI writes itself.
func (a *App) stderr() io.Writer {
	if a.errW == nil {
		return a.Stderr
	}
	return a.errW
}

// pipeClosed reports whether the run has written to a closed stdout or
// stderr.
func (a *App) pipeClosed() bool {
	return a.outW != nil && a.outW.closed.Load() || a.errW != nil && a.errW.closed.Load()
}
