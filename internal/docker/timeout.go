package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ProbeTimeout bounds one diagnostic request to the daemon, such as doctor's
// `docker info` (spec §10.2: 5 to 10 s per probe), so a hung daemon fails
// the probe instead of hanging the command.
const ProbeTimeout = 10 * time.Second

// WithTimeout returns a Runner that cancels each call to r after d. A call
// that runs out of time is cancelled like any other, so over ExecRunner it
// can return up to WaitDelay later, and it returns a *TimeoutError. Spec
// §10.2's budgets are 10 s for `compose ps` and 5–10 s per probe; long
// operations get no timeout and end only with their context.
func WithTimeout(r Runner, d time.Duration) Runner {
	return timeoutRunner{r: r, d: d}
}

// TimeoutError is a command that WithTimeout stopped. It matches
// context.DeadlineExceeded with errors.Is.
type TimeoutError struct {
	Argv    []string
	Timeout time.Duration
	// Err is the runner's error, which wraps context.DeadlineExceeded.
	Err error
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s", FormatArgv(e.Argv), e.Timeout)
}

func (e *TimeoutError) Unwrap() error { return e.Err }

// errTimedOut is the cause timeoutRunner gives its context, so it can tell
// its own deadline from the caller's.
var errTimedOut = errors.New("docker: per-call timeout")

type timeoutRunner struct {
	r Runner
	d time.Duration
}

func (t timeoutRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errTimedOut)
	defer cancel()
	res, err := t.r.Run(ctx, c)
	return res, t.check(ctx, c, err)
}

func (t timeoutRunner) Stream(ctx context.Context, c Cmd, stdout, stderr io.Writer) (int, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errTimedOut)
	defer cancel()
	code, err := t.r.Stream(ctx, c, stdout, stderr)
	return code, t.check(ctx, c, err)
}

func (t timeoutRunner) check(ctx context.Context, c Cmd, err error) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) && context.Cause(ctx) == errTimedOut {
		return &TimeoutError{Argv: c.Argv, Timeout: t.d, Err: err}
	}
	return err
}
