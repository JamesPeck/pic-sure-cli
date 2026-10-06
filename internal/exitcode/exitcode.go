// Package exitcode defines the CLI's process exit codes (spec §10.4) and an
// error type that carries one. Commands return these errors, and the cli
// package turns whatever error a command returns into the process exit code
// with FromError.
package exitcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Exit codes (spec §10.4). Agents and scripts depend on these values, so
// they never change within v2.
const (
	CodeOK              = 0   // success; status is always 0 when the stack is found
	CodeFailed          = 1   // the operation failed, including doctor with a failing check
	CodeUsage           = 2   // bad flags or arguments
	CodePrecondition    = 3   // docker missing or unsupported, stack not found, ports busy
	CodeConfirmRequired = 4   // destructive command with no TTY and no --yes
	CodeIncompatible    = 5   // CLI and stack or release versions don't match
	CodeInterrupted     = 130 // SIGINT; any other signal N exits 128+N
)

// Error is an error with an exit code.
type Error struct {
	Code int
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

func newf(code int, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// Failed reports an operation that failed (exit 1). The arguments are as for
// fmt.Errorf, so %w wraps a cause.
func Failed(format string, args ...any) error { return newf(CodeFailed, format, args...) }

// Usage reports bad flags or arguments (exit 2).
func Usage(format string, args ...any) error { return newf(CodeUsage, format, args...) }

// Precondition reports an unmet precondition (exit 3).
func Precondition(format string, args ...any) error { return newf(CodePrecondition, format, args...) }

// ConfirmRequired reports a destructive command that needs a TTY
// confirmation or --yes (exit 4).
func ConfirmRequired(format string, args ...any) error {
	return newf(CodeConfirmRequired, format, args...)
}

// Incompatible reports a version mismatch the CLI refuses to work across
// (exit 5).
func Incompatible(format string, args ...any) error { return newf(CodeIncompatible, format, args...) }

// Signaled reports that the CLI was stopped by sig (exit 128+N).
func Signaled(sig os.Signal) error {
	code := CodeInterrupted
	if s, ok := sig.(syscall.Signal); ok {
		code = 128 + int(s)
	}
	return &Error{Code: code, Err: fmt.Errorf("interrupted (%v)", sig)}
}

// FromError returns the exit code for err: 0 for nil, the code of the
// outermost *Error in the chain, 130 for a context cancellation, and 1 for
// anything else.
func FromError(err error) int {
	if err == nil {
		return CodeOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) {
		return CodeInterrupted
	}
	return CodeFailed
}
