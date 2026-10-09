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
		{"not in the docker group", exit("permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n"), false, true},
		{"earlier stderr line", exit("Cannot connect to the Docker daemon at tcp://h:2375\nbuild failed\n"), false, false},
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
			if IsComposeMissing(tc.err) {
				t.Error("IsComposeMissing = true")
			}
		})
	}
}

func TestIsComposeMissing(t *testing.T) {
	for _, stderr := range []string{
		"docker: unknown command: docker compose\n\nRun 'docker --help' for more information\n",
		"docker: 'compose' is not a docker command.\nSee 'docker --help'\n",
	} {
		if err := exitError([]string{"docker", "compose", "ps"}, 1, []byte(stderr)); !IsComposeMissing(err) {
			t.Errorf("IsComposeMissing(%v) = false", err)
		}
	}
	if IsComposeMissing(exitError([]string{"docker", "compose", "x"}, 1, []byte(`unknown docker command: "compose x"`+"\n"))) {
		t.Error("an unknown compose subcommand counts as a missing compose")
	}
}
