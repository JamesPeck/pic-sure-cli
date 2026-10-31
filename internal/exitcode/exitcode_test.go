package exitcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
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

func TestSlugOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain error", errors.New("boom"), "failed"},
		{"failed", Failed("boom"), "failed"},
		{"usage", Usage("unknown flag"), "usage"},
		{"precondition", Precondition("x"), "precondition"},
		{"incompatible", Incompatible("x"), "incompatible"},
		{"confirm required has its condition", ConfirmRequired("pass --yes"), "confirmation_required"},
		{"code 4 with no slug", &Error{Code: CodeConfirmRequired, Err: errors.New("x")}, "confirm_required"},
		{"condition", NoStack.Errorf("no stack"), "no_stack"},
		{"wrapped by fmt", fmt.Errorf("up: %w", PortsBusy.Errorf("port 80")), "ports_busy"},
		// The slug and the exit code describe the same, outermost, error.
		{"outermost slug wins", InvalidConfig.Errorf("%w", NoStack.Errorf("inner")), "invalid_config"},
		{"outermost without a slug falls back", Precondition("%w", NoStack.Errorf("inner")), "precondition"},
		{"context canceled", fmt.Errorf("waiting: %w", context.Canceled), "interrupted"},
		{"SIGINT", Signaled(syscall.SIGINT), "interrupted"},
		{"SIGPIPE", Signaled(syscall.SIGPIPE), "interrupted"},
		{"signal code with no slug", &Error{Code: 143, Err: errors.New("x")}, "interrupted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SlugOf(tt.err); got != tt.want {
				t.Errorf("SlugOf(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestConditionErrorf(t *testing.T) {
	cause := errors.New("dial unix: no such file")
	err := DockerUnavailable.Errorf("the daemon isn't reachable: %w", cause)
	if !errors.Is(err, cause) || FromError(err) != CodePrecondition || SlugOf(err) != "docker_unavailable" {
		t.Errorf("err = %v: exit %d, slug %q; want it to wrap its cause with exit 3 and docker_unavailable", err, FromError(err), SlugOf(err))
	}
}

// slugRow is a row of docs/json-schemas.md's error code table:
// | `slug` | exit | condition |
var slugRow = regexp.MustCompile("^\\| `([^`]+)` \\| ([0-9]+|128\\+N) \\|")

// TestConditionsAreDocumented keeps docs/json-schemas.md's error code table
// in step with the conditions and fallbacks: same slugs, same exit codes.
func TestConditionsAreDocumented(t *testing.T) {
	data, err := os.ReadFile("../../docs/json-schemas.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(data), "\n### Error codes\n")
	if !ok {
		t.Fatal("docs/json-schemas.md has no Error codes section")
	}
	section, _, _ = strings.Cut(section, "\n#")
	documented := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		if m := slugRow.FindStringSubmatch(line); m != nil {
			documented[m[1]] = m[2]
		}
	}
	want := map[string]string{
		"failed": "1", "usage": "2", "precondition": "3", "confirm_required": "4",
		"incompatible": "5", "interrupted": "128+N",
	}
	snake := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	for _, c := range Conditions() {
		if !snake.MatchString(c.Slug) {
			t.Errorf("slug %q isn't lower snake case", c.Slug)
		}
		if _, dup := want[c.Slug]; dup {
			t.Errorf("slug %q is used twice", c.Slug)
		}
		want[c.Slug] = strconv.Itoa(c.Code)
	}
	for slug, code := range want {
		if got, ok := documented[slug]; !ok {
			t.Errorf("%s isn't in docs/json-schemas.md's slug table", slug)
		} else if got != code {
			t.Errorf("docs/json-schemas.md gives %s exit %s, want %s", slug, got, code)
		}
	}
	for slug := range documented {
		if _, ok := want[slug]; !ok {
			t.Errorf("docs/json-schemas.md documents %s, which pic-sure doesn't use", slug)
		}
	}
}
