package docker

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestIsMissingAndIsUnreachable(t *testing.T) {
	exit := func(stderr string) error {
		return &ExitError{Argv: []string{"docker", "ps"}, ExitCode: 1, Stderr: []byte(stderr)}
	}
	for _, tc := range []struct {
		name                 string
		err                  error
		missing, unreachable bool
	}{
		{"nil", nil, false, false},
		{"docker not found", fmt.Errorf("[docker ps]: %w", &exec.Error{Name: "docker", Err: exec.ErrNotFound}), true, false},
		{"git not found", &exec.Error{Name: "git", Err: exec.ErrNotFound}, false, false},
		{"daemon error", daemonError{errors.New("x")}, false, true},
		{"stopped daemon", exit("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"), false, true},
		{"socket gone", exit("failed to connect to the docker API at unix:///nonexistent; check if the path is correct and if the daemon is running\n"), false, true},
		{"windows pipe", exit("error during connect: this error may indicate that the docker daemon is not running\n"), false, true},
		{"earlier stderr line", exit("Cannot connect to the Docker daemon at tcp://h:2375\nmore\n"), false, true},
		{"wrapped text", fmt.Errorf("step down failed: %v", exit("Cannot connect to the Docker daemon")), false, true},
		{"other failure", exit("no such service: x\n"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMissing(tc.err); got != tc.missing {
				t.Errorf("IsMissing = %v", got)
			}
			if got := IsUnreachable(tc.err); got != tc.unreachable {
				t.Errorf("IsUnreachable = %v", got)
			}
		})
	}
}
