package exitcode

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestFromError(t *testing.T) {
	cause := errors.New("docker not found")
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, CodeOK},
		{"plain error", errors.New("boom"), CodeFailed},
		{"failed", Failed("boom"), CodeFailed},
		{"usage", Usage("unknown flag %q", "--x"), CodeUsage},
		{"precondition", Precondition("%w", cause), CodePrecondition},
		{"confirm required", ConfirmRequired("pass --yes"), CodeConfirmRequired},
		{"incompatible", Incompatible("stack is newer"), CodeIncompatible},
		{"wrapped by fmt", fmt.Errorf("init: %w", Precondition("ports busy")), CodePrecondition},
		{"outermost code wins", Usage("%w", Precondition("inner")), CodeUsage},
		{"context canceled", fmt.Errorf("waiting: %w", context.Canceled), CodeInterrupted},
		{"deadline exceeded", context.DeadlineExceeded, CodeFailed},
		{"SIGINT", Signaled(syscall.SIGINT), 130},
		{"SIGTERM", Signaled(syscall.SIGTERM), 143},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromError(tt.err); got != tt.want {
				t.Errorf("FromError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestConstructorsWrapCause(t *testing.T) {
	cause := errors.New("daemon unreachable")
	err := Precondition("docker: %w", cause)
	if !errors.Is(err, cause) {
		t.Errorf("%v does not wrap its cause", err)
	}
	if got, want := err.Error(), "docker: daemon unreachable"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
