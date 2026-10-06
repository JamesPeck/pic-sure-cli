package docker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

func TestRunChecked(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Exact("docker", "volume", "rm", "demo_hpds-data")).
		Stderr("some detail\nError: volume is in use\n").Exit(1)
	f.On(fakerunner.Exact("docker", "volume", "ls")).Stdout("demo_hpds-data\n")

	res, err := docker.RunChecked(context.Background(), f, docker.Cmd{Argv: []string{"docker", "volume", "ls"}})
	if err != nil || string(res.Stdout) != "demo_hpds-data\n" {
		t.Fatalf("success: %+v, %v", res, err)
	}

	res, err = docker.RunChecked(context.Background(), f, docker.Cmd{Argv: []string{"docker", "volume", "rm", "demo_hpds-data"}})
	var exitErr *docker.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != 1 || res.ExitCode != 1 {
		t.Fatalf("failure: %+v, %v", res, err)
	}
	if want := "docker volume rm demo_hpds-data exited 1: Error: volume is in use"; err.Error() != want {
		t.Errorf("message = %q, want %q", err, want)
	}
}

func TestExitErrorMessage(t *testing.T) {
	err := &docker.ExitError{Argv: []string{"docker", "run", "--name", "", "a b"}, ExitCode: 2}
	if want := `docker run --name "" "a b" exited 2`; err.Error() != want {
		t.Errorf("no stderr: %q, want %q", err, want)
	}
	long := &docker.ExitError{Argv: []string{"git"}, ExitCode: 128, Stderr: []byte(strings.Repeat("é", 400))}
	if msg := long.Error(); !strings.HasSuffix(msg, "…") || len(msg) > 400 {
		t.Errorf("long stderr not truncated: %d bytes", len(msg))
	}
}
