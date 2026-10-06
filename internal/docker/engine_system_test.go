package docker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

func newEngine(t *testing.T) (*fakerunner.Runner, docker.Engine) {
	t.Helper()
	f := fakerunner.New(t)
	return f, docker.NewEngine(f)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The fixtures are real Docker Desktop 4.85 output, trimmed. The no-daemon
// ones were captured with DOCKER_HOST pointing at a missing socket.
const noDaemon = "failed to connect to the docker API at unix:///tmp/missing.sock; check if the path is correct and if the daemon is running: dial unix /tmp/missing.sock: connect: no such file or directory\n"

func TestVersion(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "version", "--format", "json")).Stdout(fixture(t, "version.json"))

	v, err := e.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := docker.ClientVersion{Version: "29.6.2", APIVersion: "1.55", OS: "darwin", Arch: "arm64", Context: "desktop-linux"}
	if v.Client != want {
		t.Errorf("Client = %+v, want %+v", v.Client, want)
	}
	s := v.Server
	if s == nil {
		t.Fatal("Server is nil")
	}
	if s.Platform.Name != "Docker Desktop 4.85.0 (235549)" || s.Version != "29.6.2" || s.APIVersion != "1.55" ||
		s.MinAPIVersion != "1.40" || s.OS != "linux" || s.Arch != "arm64" || s.KernelVersion != "6.12.76-linuxkit" {
		t.Errorf("Server = %+v", s)
	}
	if len(s.Components) != 4 || s.Components[1] != (docker.Component{Name: "containerd", Version: "v2.2.5"}) {
		t.Errorf("Components = %+v", s.Components)
	}
}

func TestVersionWithoutDaemon(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "version", "--format", "json")).
		Stdout(fixture(t, "version-no-daemon.json")).Stderr(noDaemon).Exit(1)

	v, err := e.Version(context.Background())
	if !errors.Is(err, docker.ErrDaemonUnreachable) {
		t.Fatalf("err = %v, want ErrDaemonUnreachable", err)
	}
	var exitErr *docker.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != 1 {
		t.Errorf("err = %#v, want the *ExitError inside", err)
	}
	if v.Server != nil || v.Client.Version != "29.6.2" || v.Client.Context != "default" {
		t.Errorf("Version = %+v, want the client half only", v)
	}
}

func TestVersionFailures(t *testing.T) {
	t.Run("docker missing", func(t *testing.T) {
		f, e := newEngine(t)
		notFound := errors.New(`exec: "docker": executable file not found in $PATH`)
		f.On(fakerunner.Glob("docker version *")).Err(notFound)
		if _, err := e.Version(context.Background()); !errors.Is(err, notFound) || errors.Is(err, docker.ErrDaemonUnreachable) {
			t.Errorf("err = %v, want the runner's error alone", err)
		}
	})
	t.Run("usage error", func(t *testing.T) {
		f, e := newEngine(t)
		f.On(fakerunner.Glob("docker version *")).Stderr("unknown flag: --format\n").Exit(125)
		_, err := e.Version(context.Background())
		if err == nil || errors.Is(err, docker.ErrDaemonUnreachable) {
			t.Errorf("err = %v, want a plain failure", err)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		f, e := newEngine(t)
		f.On(fakerunner.Glob("docker version *")).Stdout("Client: Docker Engine\n")
		if _, err := e.Version(context.Background()); err == nil {
			t.Error("unparseable output: no error")
		}
	})
}

func TestInfo(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "info", "--format", "json")).Stdout(fixture(t, "info.json"))

	info, err := e.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerVersion != "29.6.2" || info.OperatingSystem != "Docker Desktop" || info.OSType != "linux" ||
		info.Architecture != "aarch64" || info.NCPU != 14 || info.MemTotal != 29380464640 ||
		info.DockerRootDir != "/var/lib/docker" || info.Name != "docker-desktop" {
		t.Errorf("Info = %+v", info)
	}
	if info.HTTPProxy != "http.docker.internal:3128" || info.HTTPSProxy != "http.docker.internal:3128" || info.NoProxy != "hubproxy.docker.internal" {
		t.Errorf("proxies = %q %q %q", info.HTTPProxy, info.HTTPSProxy, info.NoProxy)
	}
	if len(info.SecurityOptions) != 2 || info.SecurityOptions[0] != "name=seccomp,profile=builtin" {
		t.Errorf("SecurityOptions = %q", info.SecurityOptions)
	}
	ci := info.ClientInfo
	if ci.Context != "desktop-linux" || len(ci.Plugins) != 2 {
		t.Fatalf("ClientInfo = %+v", ci)
	}
	if p := ci.Plugins[1]; p.Name != "compose" || p.Version != "v5.3.1" || p.Path != "/Users/someone/.docker/cli-plugins/docker-compose" {
		t.Errorf("compose plugin = %+v", p)
	}
}

func TestInfoWithoutDaemon(t *testing.T) {
	t.Run("current CLI", func(t *testing.T) {
		f, e := newEngine(t)
		f.On(fakerunner.Exact("docker", "info", "--format", "json")).
			Stdout(fixture(t, "info-no-daemon.json")).Stderr(noDaemon).Exit(1)
		info, err := e.Info(context.Background())
		if !errors.Is(err, docker.ErrDaemonUnreachable) {
			t.Fatalf("err = %v, want ErrDaemonUnreachable", err)
		}
		if info.ClientInfo.Context != "default" || len(info.ClientInfo.Plugins) != 2 {
			t.Errorf("ClientInfo = %+v, want what the client knows", info.ClientInfo)
		}
	})
	t.Run("older CLI", func(t *testing.T) {
		// Older CLIs exit 0 from docker info --format json and report the
		// daemon's absence in ServerErrors.
		f, e := newEngine(t)
		f.On(fakerunner.Exact("docker", "info", "--format", "json")).
			Stdout(`{"ServerVersion":"","ClientInfo":{"Context":"colima"},"ServerErrors":["Cannot connect to the Docker daemon at unix:///Users/someone/.colima/default/docker.sock. Is the docker daemon running?"]}`)
		info, err := e.Info(context.Background())
		if !errors.Is(err, docker.ErrDaemonUnreachable) || err.Error() != info.ServerErrors[0] {
			t.Errorf("err = %v, want ErrDaemonUnreachable with the server error", err)
		}
		if info.ClientInfo.Context != "colima" {
			t.Errorf("Context = %q", info.ClientInfo.Context)
		}
	})
}
