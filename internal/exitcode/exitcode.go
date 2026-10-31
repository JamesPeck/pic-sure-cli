// Package exitcode defines the CLI's process exit codes (spec §10.4) and an
// error type that carries one, with a slug naming the condition. Commands
// return these errors, and the cli package turns whatever error a command
// returns into the process exit code with FromError, and into the JSON
// error's code with SlugOf.
package exitcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
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
	// Slug names the condition for --json callers (see Condition); "" means
	// SlugOf falls back to the exit code's slug.
	Slug string
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
// confirmation or --yes (exit 4), as ConfirmationRequired.
func ConfirmRequired(format string, args ...any) error {
	return ConfirmationRequired.Errorf(format, args...)
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
	return &Error{Code: code, Slug: slugInterrupted, Err: fmt.Errorf("interrupted (%v)", sig)}
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

// SlugOf returns the slug for err: "" for nil, else the slug of the
// outermost *Error in the chain, the error FromError takes the exit code
// from. When that error has no slug, or there is none, the slug is the exit
// code's: failed, usage, precondition, confirm_required, incompatible or
// interrupted.
func SlugOf(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) && e.Slug != "" {
		return e.Slug
	}
	switch code := FromError(err); {
	case code == CodeUsage:
		return "usage"
	case code == CodePrecondition:
		return "precondition"
	case code == CodeConfirmRequired:
		return "confirm_required"
	case code == CodeIncompatible:
		return "incompatible"
	case code > 128:
		return slugInterrupted
	}
	return "failed"
}

const slugInterrupted = "interrupted"

// A Condition is a failure that --json callers can tell apart from others
// with the same exit code: a slug, which never changes within v2, and the
// exit code that goes with it. docs/json-schemas.md lists them.
type Condition struct {
	Slug string
	Code int
}

// Errorf returns an *Error for c. The arguments are as for fmt.Errorf, so %w
// wraps a cause.
func (c Condition) Errorf(format string, args ...any) error {
	return &Error{Code: c.Code, Slug: c.Slug, Err: fmt.Errorf(format, args...)}
}

// The conditions, in exit code order.
var (
	StepFailed  = condition("step_failed", CodeFailed)
	StackLocked = condition("stack_locked", CodeFailed)

	InvalidConfig = condition("invalid_config", CodeUsage)
	InvalidSecret = condition("invalid_secret", CodeUsage)

	DockerUnavailable   = condition("docker_unavailable", CodePrecondition)
	ComposeNotInstalled = condition("compose_not_installed", CodePrecondition)
	NoStack             = condition("no_stack", CodePrecondition)
	WrongOwner          = condition("wrong_owner", CodePrecondition)
	StackNotInitialised = condition("stack_not_initialised", CodePrecondition)
	StackNotRendered    = condition("stack_not_rendered", CodePrecondition)
	PortsBusy           = condition("ports_busy", CodePrecondition)
	GeneratedOpenSecret = condition("generated_open_secret", CodePrecondition)
	ForeignResources    = condition("foreign_resources", CodePrecondition)

	ConfirmationRequired = condition("confirmation_required", CodeConfirmRequired)

	CLITooOld               = condition("cli_too_old", CodeIncompatible)
	ConfigMigrationsPending = condition("config_migrations_pending", CodeIncompatible)
)

var conditions []Condition

func condition(slug string, code int) Condition {
	c := Condition{Slug: slug, Code: code}
	conditions = append(conditions, c)
	return c
}

// Conditions returns every Condition, in exit code order.
func Conditions() []Condition { return slices.Clone(conditions) }
