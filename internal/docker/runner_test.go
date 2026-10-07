package docker_test

import (
	"context"
	"errors"
	"fmt"
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

func TestPortAllocated(t *testing.T) {
	for stderr, want := range map[string]int{
		"Error response from daemon: driver failed programming external connectivity on endpoint demo-httpd-1 (0123): Bind for 0.0.0.0:8080 failed: port is already allocated": 8080,
		"Error response from daemon: Ports are not available: exposing port TCP 0.0.0.0:8443 -> 127.0.0.1:0: listen tcp 0.0.0.0:8443: bind: address already in use":            8443,
		"Error response from daemon: Bind for [::]:15003 failed: port is already allocated":                                                                                    15003,
		"no such container: demo-httpd-1": 0,
	} {
		err := fmt.Errorf("start: %w", &docker.ExitError{Argv: []string{"docker", "compose", "up"}, ExitCode: 1, Stderr: []byte("Container demo-httpd-1 Starting\n" + stderr + "\n")})
		if got := docker.PortAllocated(err); got != want {
			t.Errorf("PortAllocated(%q) = %d, want %d", stderr, got, want)
		}
	}
	if docker.PortAllocated(errors.New("Bind for 0.0.0.0:8080 failed: port is already allocated")) != 0 {
		t.Error("PortAllocated matched an error that isn't docker's")
	}
}
