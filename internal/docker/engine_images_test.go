package docker_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

const (
	img          = "hms-dbmi/pic-sure-hpds:0123456789ab"
	imgInspect   = `[{"Id":"sha256:33bee74c","RepoTags":["hms-dbmi/pic-sure-hpds:0123456789ab"],"Config":{"Labels":{"org.hms-dbmi.picsure.reactor-src":"0123456789abcdef"}}}]`
	noSuchImage  = "Error response from daemon: No such image: hms-dbmi/pic-sure-hpds:0123456789ab\n"
	daemonDenied = "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n"
)

func TestImageInspection(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "image", "inspect", img)).Stdout(imgInspect)
	ctx := context.Background()

	if ok, err := e.ImageExists(ctx, img); !ok || err != nil {
		t.Errorf("ImageExists = %v, %v", ok, err)
	}
	if id, err := e.ImageID(ctx, img); id != "sha256:33bee74c" || err != nil {
		t.Errorf("ImageID = %q, %v", id, err)
	}
	labels, err := e.ImageLabels(ctx, img)
	if want := map[string]string{"org.hms-dbmi.picsure.reactor-src": "0123456789abcdef"}; !maps.Equal(labels, want) || err != nil {
		t.Errorf("ImageLabels = %v, %v", labels, err)
	}
}

func TestImageWithoutLabels(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "image", "inspect", "alpine")).Stdout(`[{"Id":"sha256:1","Config":{"Labels":null}}]`)
	labels, err := e.ImageLabels(context.Background(), "alpine")
	if err != nil || labels == nil || len(labels) != 0 {
		t.Errorf("ImageLabels = %#v, %v, want an empty map", labels, err)
	}
}

func TestMissingImage(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "image", "inspect", img)).Stdout("[]\n").Stderr(noSuchImage).Exit(1)
	ctx := context.Background()

	if ok, err := e.ImageExists(ctx, img); ok || err != nil {
		t.Errorf("ImageExists = %v, %v, want false, nil", ok, err)
	}
	if _, err := e.ImageID(ctx, img); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("ImageID err = %v, want ErrNotFound", err)
	}
	if _, err := e.ImageLabels(ctx, img); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("ImageLabels err = %v, want ErrNotFound", err)
	}
}

func TestImageExistsReportsOtherFailures(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker image inspect *")).Stderr(daemonDenied).Exit(1)
	ok, err := e.ImageExists(context.Background(), img)
	if ok || err == nil || errors.Is(err, docker.ErrNotFound) {
		t.Errorf("ImageExists = %v, %v, want the daemon error", ok, err)
	}
}

func TestBuild(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "build",
		"-f", "/cache/build/0123456789ab/services/hpds/Dockerfile",
		"-t", img,
		"--label", "org.hms-dbmi.picsure.reactor-src=0123456789abcdef",
		"--label", "org.hms-dbmi.picsure.stack=demo",
		"--build-arg", "HTTP_PROXY", "--build-arg", "http_proxy",
		"/cache/build/0123456789ab/services/hpds",
	)).Stdout("#1 [internal] load build definition\n").Stderr("#5 DONE 0.1s\n")

	var out, errOut bytes.Buffer
	err := e.Build(context.Background(), docker.BuildOpts{
		Context: "/cache/build/0123456789ab/services/hpds",
		File:    "/cache/build/0123456789ab/services/hpds/Dockerfile",
		Tag:     img,
		Labels: map[string]string{
			"org.hms-dbmi.picsure.stack":       "demo",
			"org.hms-dbmi.picsure.reactor-src": "0123456789abcdef",
		},
		BuildArgs: []string{"HTTP_PROXY=http://user:hunter2@proxy:3128", "http_proxy=http://user:hunter2@proxy:3128"},
		Stdout:    &out,
		Stderr:    &errOut,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "#1 [internal] load build definition\n" || errOut.String() != "#5 DONE 0.1s\n" {
		t.Errorf("streamed stdout %q, stderr %q", out.String(), errOut.String())
	}
	c := f.Calls()[0]
	if !c.HasEnv("HTTP_PROXY") || !c.HasEnv("http_proxy") {
		t.Errorf("Env = %v, want the build args' values passed by environment", c.Env)
	}
}

func TestBuildMinimal(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "build", "-t", "hms-dbmi/dictionary-etl:0123456789ab", "/cache/src/etl"))
	if err := e.Build(context.Background(), docker.BuildOpts{Context: "/cache/src/etl", Tag: "hms-dbmi/dictionary-etl:0123456789ab"}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildFailure(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker build *")).
		Stderr("#7 ERROR: process \"/bin/sh -c apk add curl\" did not complete successfully: exit code: 1\nERROR: failed to solve: unable to select packages\n").
		Exit(1)
	var log bytes.Buffer
	err := e.Build(context.Background(), docker.BuildOpts{Context: "/ctx", Tag: "t:1", Stderr: &log})
	var exitErr *docker.ExitError
	if !errors.As(err, &exitErr) || !strings.HasSuffix(err.Error(), "exited 1: ERROR: failed to solve: unable to select packages") {
		t.Errorf("err = %v, want an *ExitError naming the last stderr line", err)
	}
	if !strings.Contains(log.String(), "#7 ERROR") {
		t.Errorf("stderr not streamed: %q", log.String())
	}
}

func TestBuildRejects(t *testing.T) {
	_, e := newEngine(t)
	for name, opts := range map[string]docker.BuildOpts{
		"no tag":            {Context: "/ctx"},
		"relative context":  {Context: "ctx", Tag: "t:1"},
		"relative file":     {Context: "/ctx", File: "Dockerfile", Tag: "t:1"},
		"bare build arg":    {Context: "/ctx", Tag: "t:1", BuildArgs: []string{"HTTP_PROXY"}},
		"docker's own name": {Context: "/ctx", Tag: "t:1", BuildArgs: []string{"DOCKER_HOST=tcp://x"}},
	} {
		if err := e.Build(context.Background(), opts); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestPull(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "pull", "alpine:3.20")).Stdout("3.20: Pulling from library/alpine\nStatus: Downloaded newer image for alpine:3.20\n")
	f.On(fakerunner.Exact("docker", "pull", "hms-dbmi/missing:1")).
		Stdout("Using default tag\n").
		Stderr("Error response from daemon: pull access denied for hms-dbmi/missing, repository does not exist or may require 'docker login'\n").
		Exit(1)

	var out bytes.Buffer
	if err := e.Pull(context.Background(), "alpine:3.20", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Downloaded newer image") {
		t.Errorf("progress = %q", out.String())
	}
	err := e.Pull(context.Background(), "hms-dbmi/missing:1", nil)
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Errorf("err = %v, want docker's message", err)
	}
}

func TestRemoveImage(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "image", "rm", img)).Times(1)
	f.On(fakerunner.Exact("docker", "image", "rm", img)).Stderr(noSuchImage).Exit(1).Times(1)
	f.On(fakerunner.Exact("docker", "image", "rm", img)).
		Stderr("Error response from daemon: conflict: unable to remove repository reference \"" + img + "\" (must force) - container 5f3 is using its referenced image 33b\n").
		Exit(1)
	ctx := context.Background()

	if err := e.RemoveImage(ctx, img); err != nil {
		t.Errorf("present: %v", err)
	}
	if err := e.RemoveImage(ctx, img); err != nil {
		t.Errorf("already gone: %v, want nil", err)
	}
	if err := e.RemoveImage(ctx, img); err == nil || !strings.Contains(err.Error(), "is using its referenced image") {
		t.Errorf("in use: %v, want docker's conflict", err)
	}
}

func TestTag(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "tag", "ghcr.io/"+img, img)).Times(1)
	f.On(fakerunner.Exact("docker", "tag", "ghcr.io/"+img, img)).Stderr(noSuchImage).Exit(1)
	ctx := context.Background()

	if err := e.Tag(ctx, "ghcr.io/"+img, img); err != nil {
		t.Errorf("present: %v", err)
	}
	if err := e.Tag(ctx, "ghcr.io/"+img, img); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("missing source: %v, want ErrNotFound", err)
	}
}
