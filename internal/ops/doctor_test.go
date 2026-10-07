package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/git"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

const gib = 1 << 30

// fakeHost is an ops.Host with everything present, free and reachable.
type fakeHost struct {
	missing  map[string]bool
	diskFree uint64
	diskErr  error
	busy     map[int]bool
	reach    func(rawURL string) (int, error)
	proxied  []string // URLs Reach sent through a proxy
}

func (h *fakeHost) LookPath(name string) (string, error) {
	if h.missing[name] {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + name, nil
}

func (h *fakeHost) DiskFree(string) (uint64, error) { return h.diskFree, h.diskErr }
func (h *fakeHost) PortFree(p int) bool             { return !h.busy[p] }

func (h *fakeHost) Reach(_ context.Context, rawURL string, proxy func(*http.Request) (*url.URL, error)) (int, error) {
	req, _ := http.NewRequest(http.MethodHead, rawURL, nil)
	if u, _ := proxy(req); u != nil {
		h.proxied = append(h.proxied, rawURL)
	}
	if h.reach != nil {
		return h.reach(rawURL)
	}
	return http.StatusOK, nil
}

// fakeCompose answers Config and Ps; nothing else is called.
type fakeCompose struct {
	docker.Composer
	configErr error
	ps        []docker.ComposeService
}

func (c *fakeCompose) Config(context.Context, bool) ([]byte, error) { return nil, c.configErr }
func (c *fakeCompose) Ps(context.Context, ...string) ([]docker.ComposeService, error) {
	return c.ps, nil
}

// fakeGit answers LsRemote and records the env WithEnv added.
type fakeGit struct {
	git.Client
	err error
	env []string
}

func (g *fakeGit) WithEnv(env ...string) git.Client { g.env = env; return g }
func (g *fakeGit) LsRemote(context.Context, string) ([]git.Ref, error) {
	return []git.Ref{{Name: "refs/heads/main"}}, g.err
}

type doctorEnv struct {
	t       *testing.T
	f       *fakerunner.Runner
	host    *fakeHost
	git     *fakeGit
	compose *fakeCompose
	info    docker.Info
	version docker.VersionInfo
	dfAvail uint64 // KiB the probe container's df reports
	opts    ops.DoctorOptions
}

func newDoctorEnv(t *testing.T) *doctorEnv {
	e := &doctorEnv{
		t:    t,
		f:    fakerunner.New(t),
		host: &fakeHost{diskFree: 100 * gib},
		git:  &fakeGit{},
		info: docker.Info{
			ServerVersion: "28.5.1", OperatingSystem: "Docker Desktop", OSType: "linux",
			Architecture: "x86_64", MemTotal: 16 * gib, DockerRootDir: "/var/lib/docker",
			ClientInfo: docker.ClientInfo{Context: "desktop-linux", Plugins: []docker.Plugin{
				{Name: "compose", Version: "v2.29.7"}, {Name: "buildx", Version: "v0.17.1-desktop.1"},
			}},
		},
		version: docker.VersionInfo{Server: &docker.ServerVersion{Platform: docker.Platform{Name: "Docker Desktop 4.48.0"}}},
		dfAvail: 100 << 20,
	}
	e.opts = ops.DoctorOptions{Host: e.host, CacheDir: "/home/u/.cache/pic-sure"}
	return e
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// run adds the default docker answers after any the test added, then runs
// Doctor.
func (e *doctorEnv) run() *ops.DoctorReport {
	e.t.Helper()
	e.f.On(fakerunner.Exact("docker", "info", "--format", "json")).Stdout(jsonOf(e.t, e.info))
	e.f.On(fakerunner.Exact("docker", "version", "--format", "json")).Stdout(jsonOf(e.t, e.version))
	e.f.On(fakerunner.Glob("docker image inspect alpine:*")).Stdout(`[{"Id":"sha256:a1"}]`)
	e.f.On(fakerunner.Glob("docker run * df -Pk /")).Stdout(fmt.Sprintf(
		"Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 200000000 1000 %d 1%% /\n", e.dfAvail))
	e.f.On(fakerunner.Glob("docker rm -v -f pic-sure-doctor-*")).Exit(1).Stderr("Error response from daemon: No such container: x\n")
	e.f.On(fakerunner.Glob("docker ps *")).Stdout("")
	e.f.On(fakerunner.Glob("docker image inspect *")).Exit(1).Stderr("Error: No such image\n")
	e.f.On(fakerunner.Glob("docker pull *"))
	d := &ops.Deps{
		Runner: e.f, Docker: docker.NewEngine(e.f), Git: e.git,
		Rand: strings.NewReader(strings.Repeat("r", 64)), Sink: events.Discard,
	}
	if e.compose != nil {
		d.Compose = e.compose
	}
	return ops.Doctor(context.Background(), d, e.opts)
}

func check(t *testing.T, r *ops.DoctorReport, name string) ops.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, r.Checks)
	return ops.Check{}
}

func wantCheck(t *testing.T, r *ops.DoctorReport, name string, status ops.CheckStatus, msg string) ops.Check {
	t.Helper()
	c := check(t, r, name)
	if c.Status != status || !strings.Contains(c.Message, msg) {
		t.Errorf("%s = %s %q, want %s containing %q", name, c.Status, c.Message, status, msg)
	}
	return c
}

func noCheck(t *testing.T, r *ops.DoctorReport, name string) {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			t.Errorf("unexpected check %+v", c)
		}
	}
}

func TestDoctorHostAllOK(t *testing.T) {
	e := newDoctorEnv(t)
	r := e.run()
	if r.Failed() {
		t.Errorf("Failed() with %+v", r.Checks)
	}
	var names []string
	for _, c := range r.Checks {
		names = append(names, c.Name)
		if c.Status != ops.CheckOK {
			t.Errorf("%s = %s %q, want ok", c.Name, c.Status, c.Message)
		}
	}
	want := "docker-cli docker-daemon compose-version buildx-version docker-runtime git disk-cache disk-docker memory arm64-images"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("checks = %s\nwant     %s", got, want)
	}
	// The disk probe is a uniquely named, self-removing, offline container,
	// removed again in case --rm didn't run.
	e.f.AssertCalled(fakerunner.Regex(`^docker run .*--rm --name pic-sure-doctor-\w+ --network none .*alpine:\S+ df -Pk /$`))
	e.f.AssertCalled(fakerunner.Glob("docker rm -v -f pic-sure-doctor-*"))
	e.f.AssertNotCalled(fakerunner.Glob("docker pull *"))
}

func TestDoctorDockerMissing(t *testing.T) {
	e := newDoctorEnv(t)
	e.host.missing = map[string]bool{"docker": true}
	r := e.run()
	wantCheck(t, r, "docker-cli", ops.CheckFail, "not on PATH")
	noCheck(t, r, "docker-daemon")
	noCheck(t, r, "disk-docker")
	wantCheck(t, r, "git", ops.CheckOK, "")
	if !r.Failed() {
		t.Error("Failed() = false")
	}
	if n := len(e.f.Calls()); n != 0 {
		t.Errorf("ran %d docker commands without docker", n)
	}
}

func TestDoctorDaemonUnreachable(t *testing.T) {
	e := newDoctorEnv(t)
	e.f.On(fakerunner.Exact("docker", "info", "--format", "json")).Exit(1).
		Stdout(`{"ClientInfo":{"Context":"colima","Plugins":[{"Name":"compose","Version":"v2.31.0"}]}}`).
		Stderr("Cannot connect to the Docker daemon at unix:///x/docker.sock. Is the docker daemon running?\n")
	r := e.run()
	c := wantCheck(t, r, "docker-daemon", ops.CheckFail, "isn't reachable")
	if c.Detail == "" {
		t.Error("no detail on how to start Docker")
	}
	wantCheck(t, r, "compose-version", ops.CheckOK, "v2.31.0")
	wantCheck(t, r, "buildx-version", ops.CheckWarn, "isn't installed")
	for _, name := range []string{"docker-runtime", "disk-docker", "memory", "arm64-images"} {
		noCheck(t, r, name)
	}
}

func TestDoctorPluginVersions(t *testing.T) {
	for _, tc := range []struct {
		name, plugin, version string
		building              bool
		check                 string
		want                  ops.CheckStatus
		msg                   string
	}{
		{"compose minimum", "compose", "v2.29.0", false, "compose-version", ops.CheckOK, "v2.29.0"},
		{"compose desktop build", "compose", "2.40.3-desktop.1", false, "compose-version", ops.CheckOK, ""},
		{"compose too old", "compose", "v2.28.1", false, "compose-version", ops.CheckFail, "older than 2.29.0"},
		{"compose v1", "compose", "1.29.2", false, "compose-version", ops.CheckFail, "older"},
		{"compose missing", "compose", "", false, "compose-version", ops.CheckFail, "isn't installed"},
		{"compose unparsable", "compose", "dev", false, "compose-version", ops.CheckWarn, "can't parse"},
		{"buildx old", "buildx", "v0.16.2", false, "buildx-version", ops.CheckWarn, "older than 0.17.0"},
		{"buildx old when building", "buildx", "v0.16.2", true, "buildx-version", ops.CheckFail, "older than 0.17.0"},
		{"buildx missing when building", "buildx", "", true, "buildx-version", ops.CheckFail, "isn't installed"},
		{"buildx ok", "buildx", "v0.29.1", true, "buildx-version", ops.CheckOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDoctorEnv(t)
			e.opts.Building = tc.building
			var plugins []docker.Plugin
			for _, p := range e.info.ClientInfo.Plugins {
				if p.Name != tc.plugin {
					plugins = append(plugins, p)
				}
			}
			if tc.version != "" {
				plugins = append(plugins, docker.Plugin{Name: tc.plugin, Version: tc.version})
			}
			e.info.ClientInfo.Plugins = plugins
			wantCheck(t, e.run(), tc.check, tc.want, tc.msg)
		})
	}
}

func TestDoctorRuntime(t *testing.T) {
	for _, tc := range []struct {
		name      string
		os, ctx   string
		platform  string
		want      string
		wantState ops.CheckStatus
	}{
		{"desktop", "Docker Desktop", "desktop-linux", "Docker Desktop 4.48.0", "Docker Desktop", ops.CheckOK},
		{"orbstack", "OrbStack", "orbstack", "", "OrbStack", ops.CheckOK},
		{"colima", "Ubuntu 24.04 LTS", "colima", "Docker Engine - Community", "Colima", ops.CheckOK},
		{"podman", "fedora", "default", "Podman Engine", "Podman", ops.CheckWarn},
		{"engine", "Ubuntu 24.04.1 LTS", "default", "Docker Engine - Community", "Docker Engine", ops.CheckOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDoctorEnv(t)
			e.info.OperatingSystem, e.info.ClientInfo.Context = tc.os, tc.ctx
			e.version.Server.Platform.Name = tc.platform
			wantCheck(t, e.run(), "docker-runtime", tc.wantState, tc.want+" (context "+tc.ctx+")")
		})
	}
}

func TestDoctorGitMissing(t *testing.T) {
	e := newDoctorEnv(t)
	e.host.missing = map[string]bool{"git": true}
	wantCheck(t, e.run(), "git", ops.CheckFail, "not on PATH")
}

func TestDoctorDisk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		free  uint64
		state ops.CheckStatus
	}{
		{"plenty", 21 * gib, ops.CheckOK},
		{"low", 19 * gib, ops.CheckWarn},
		{"too low", 4 * gib, ops.CheckFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDoctorEnv(t)
			e.host.diskFree = tc.free
			e.dfAvail = tc.free >> 10
			r := e.run()
			wantCheck(t, r, "disk-cache", tc.state, "/home/u/.cache/pic-sure")
			wantCheck(t, r, "disk-docker", tc.state, "/var/lib/docker")
		})
	}
	t.Run("no cache", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.opts.CacheDir, e.opts.CacheErr = "", errors.New("cache root is inside TMPDIR")
		wantCheck(t, e.run(), "disk-cache", ops.CheckFail, "inside TMPDIR")
	})
	t.Run("cache unmeasurable", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.host.diskErr = errors.New("statfs: boom")
		wantCheck(t, e.run(), "disk-cache", ops.CheckWarn, "statfs: boom")
	})
	t.Run("alpine not pulled", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.f.On(fakerunner.Glob("docker image inspect alpine:*")).Exit(1).Stderr("Error response from daemon: No such image: alpine:3.23\n")
		wantCheck(t, e.run(), "disk-docker", ops.CheckWarn, "isn't pulled yet")
		e.f.AssertNotCalled(fakerunner.Glob("docker run *"))
	})
	t.Run("probe fails", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.f.On(fakerunner.Glob("docker run * df -Pk /")).Exit(125).Stderr("docker: Error response from daemon: pull access denied\n")
		wantCheck(t, e.run(), "disk-docker", ops.CheckWarn, "pull access denied")
		e.f.AssertCalled(fakerunner.Glob("docker rm -v -f pic-sure-doctor-*"))
	})
}

func hpdsInspect(t *testing.T, containers ...[3]string) string {
	var out []map[string]any
	for _, c := range containers {
		out = append(out, map[string]any{"Name": "/x", "Config": map[string]any{
			"Env":    []string{"PATH=/bin", "JAVA_OPTS=" + c[2]},
			"Labels": map[string]string{stack.LabelStack: c[0], stack.LabelStackDir: c[1]},
		}})
	}
	return jsonOf(t, out)
}

func TestDoctorMemory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		heaps []string
		state ops.CheckStatus
		msg   string
	}{
		{"none running", nil, ops.CheckOK, "no stack's HPDS is running"},
		{"fits", []string{"-Xms1g -Xmx4g", "-Xmx2048m"}, ops.CheckOK, "HPDS heaps total 6.0 GiB (a 4.0 GiB, b 2.0 GiB)"},
		{"tight", []string{"-Xmx13g"}, ops.CheckWarn, "leaving under 4.0 GiB"},
		{"over", []string{"-Xmx8g", "-Xmx2g -Xmx9g"}, ops.CheckFail, "HPDS heaps total 17.0 GiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDoctorEnv(t)
			if len(tc.heaps) > 0 {
				var ids []string
				var cs [][3]string
				for i, h := range tc.heaps {
					ids = append(ids, fmt.Sprintf("id%d", i))
					cs = append(cs, [3]string{string(rune('a' + i)), "/stacks/" + string(rune('a'+i)), h})
				}
				e.f.On(fakerunner.Glob("docker ps --quiet --no-trunc --filter label=org.hms-dbmi.picsure.stack --filter label=com.docker.compose.service=hpds")).Stdout(strings.Join(ids, "\n") + "\n")
				e.f.On(fakerunner.Exact(append([]string{"docker", "container", "inspect"}, ids...)...)).Stdout(hpdsInspect(t, cs...))
			}
			c := wantCheck(t, e.run(), "memory", tc.state, tc.msg)
			if !strings.HasPrefix(c.Message, "Docker has 16.0 GiB") {
				t.Errorf("message %q doesn't start with the memory", c.Message)
			}
		})
	}
	t.Run("this stack counted when down", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, func(c *stack.Config) { c.HPDS.JavaOpts = "-Xmx16g" })
		wantCheck(t, e.run(), "memory", ops.CheckWarn, "this stack 16.0 GiB when up")
	})
	t.Run("a stack init hasn't created yet", func(t *testing.T) {
		e := newDoctorEnv(t)
		cfg := stack.DefaultConfig()
		cfg.HPDS.JavaOpts = "-Xmx20g"
		e.opts.Config = &cfg
		wantCheck(t, e.run(), "memory", ops.CheckWarn, "this stack 20.0 GiB when up), more than Docker has")
	})
	t.Run("a stack init resumes, already running", func(t *testing.T) {
		e := newDoctorEnv(t)
		cfg := stack.DefaultConfig()
		cfg.Name, cfg.HPDS.JavaOpts = "demo", "-Xmx16g"
		e.opts.Config = &cfg
		e.f.On(fakerunner.Glob("docker ps *")).Stdout("id0\n")
		e.f.On(fakerunner.Exact("docker", "container", "inspect", "id0")).Stdout(
			`[{"Config":{"Env":["JAVA_OPTS=-Xmx16g"],"Labels":{"` + stack.LabelStack + `":"demo"}}}]`)
		wantCheck(t, e.run(), "memory", ops.CheckWarn, "HPDS heaps total 16.0 GiB (demo (this stack) 16.0 GiB)")
	})
	t.Run("this stack over when down only warns", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, func(c *stack.Config) { c.HPDS.JavaOpts = "-Xmx20g" })
		wantCheck(t, e.run(), "memory", ops.CheckWarn, "this stack 20.0 GiB when up), more than Docker has")
	})
	t.Run("this stack counted once when up", func(t *testing.T) {
		e := newDoctorEnv(t)
		st := e.stack(t, func(c *stack.Config) { c.HPDS.JavaOpts = "-Xmx16g" })
		e.f.On(fakerunner.Glob("docker ps *")).Stdout("id0\n")
		e.f.On(fakerunner.Glob("docker container inspect id0")).Stdout(hpdsInspect(t, [3]string{"demo", st.Dir, "-Xmx2g"}))
		wantCheck(t, e.run(), "memory", ops.CheckOK, "(demo (this stack) 2.0 GiB)")
	})
}

func TestDoctorArm64Images(t *testing.T) {
	t.Run("amd64 daemon", func(t *testing.T) {
		e := newDoctorEnv(t)
		wantCheck(t, e.run(), "arm64-images", ops.CheckOK, "not an arm64 daemon")
		e.f.AssertNotCalled(fakerunner.Glob("docker image inspect --format *"))
	})
	t.Run("emulated image", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.info.Architecture = "aarch64"
		e.f.On(fakerunner.Glob("docker image inspect --format {{.Architecture}} mysql:*")).Stdout("amd64\n")
		e.f.On(fakerunner.Glob("docker image inspect --format {{.Architecture}} alpine:*")).Stdout("arm64\n")
		c := wantCheck(t, e.run(), "arm64-images", ops.CheckWarn, "mysql:8.0 (amd64)")
		if strings.Contains(c.Message, "alpine") {
			t.Errorf("message names the arm64 image: %q", c.Message)
		}
	})
	t.Run("inspect fails", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.info.Architecture = "aarch64"
		e.f.On(fakerunner.Glob("docker image inspect --format {{.Architecture}} mysql:*")).Exit(1).Stderr("permission denied while trying to connect to the Docker daemon socket\n")
		wantCheck(t, e.run(), "arm64-images", ops.CheckWarn, "couldn't inspect mysql:8.0: docker image inspect")
	})
	t.Run("native", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.info.Architecture = "aarch64"
		e.f.On(fakerunner.Glob("docker image inspect --format {{.Architecture}} alpine:*")).Stdout("arm64\n")
		wantCheck(t, e.run(), "arm64-images", ops.CheckOK, "pulled images are arm64: alpine:")
	})
}

// stack makes a valid open-mode stack, as edited by edit, and points the
// options at it with a rendered, valid compose file.
func (e *doctorEnv) stack(t *testing.T, edit func(*stack.Config)) *stack.Stack {
	t.Helper()
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := stack.DefaultConfig()
	cfg.Name = "demo"
	cfg.Auth.Mode = stack.AuthOpen
	cfg.Auth.AdminEmail = "admin@example.com"
	cfg.Network.HTTPPort, cfg.Network.HTTPSPort = 8080, 8443
	if edit != nil {
		edit(&cfg)
	}
	writeConfig(t, st, &cfg)
	e.opts.Stack = st
	e.compose = &fakeCompose{}
	return st
}

func writeConfig(t *testing.T, st *stack.Stack, cfg *stack.Config) {
	t.Helper()
	doc, err := stack.NewConfigDoc(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteConfig(b); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorStackOK(t *testing.T) {
	e := newDoctorEnv(t)
	st := e.stack(t, nil)
	r := e.run()
	if r.Stack != st.Dir {
		t.Errorf("Stack = %q, want %q", r.Stack, st.Dir)
	}
	for _, name := range []string{"config", "compose-config", "ports", "auth0", "proxy"} {
		wantCheck(t, r, name, ops.CheckOK, "")
	}
	noCheck(t, r, "overrides")
	if r.Failed() {
		t.Errorf("Failed() with %+v", r.Checks)
	}
	// images.mode build (the default) needs buildx.
	if c := check(t, r, "buildx-version"); c.Status != ops.CheckOK {
		t.Errorf("buildx-version = %+v", c)
	}
}

func TestDoctorStackBuildNeedsBuildx(t *testing.T) {
	e := newDoctorEnv(t)
	e.stack(t, nil)
	e.info.ClientInfo.Plugins = e.info.ClientInfo.Plugins[:1]
	wantCheck(t, e.run(), "buildx-version", ops.CheckFail, "isn't installed")
}

func TestDoctorStackConfigInvalid(t *testing.T) {
	e := newDoctorEnv(t)
	st := e.stack(t, nil)
	if err := st.WriteConfig([]byte("schema: 1\nname: demo\nbogus: 1\n")); err != nil {
		t.Fatal(err)
	}
	r := e.run()
	wantCheck(t, r, "config", ops.CheckFail, "bogus")
	wantCheck(t, r, "compose-config", ops.CheckOK, "")
	for _, name := range []string{"ports", "auth0", "proxy"} {
		noCheck(t, r, name)
	}
}

func TestDoctorStackConfigMissingFile(t *testing.T) {
	e := newDoctorEnv(t)
	e.stack(t, func(c *stack.Config) { c.TLS.Mode = stack.TLSProvided })
	wantCheck(t, e.run(), "config", ops.CheckFail, "server.crt")
}

func TestDoctorStackCompose(t *testing.T) {
	t.Run("not rendered", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, nil)
		e.compose = nil
		e.opts.ComposeErr = fmt.Errorf("%w: compose.yaml does not exist", docker.ErrNotRendered)
		wantCheck(t, e.run(), "compose-config", ops.CheckWarn, "pic-sure up")
	})
	t.Run("unreadable", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, nil)
		e.compose = nil
		e.opts.ComposeErr = errors.New("permission denied")
		wantCheck(t, e.run(), "compose-config", ops.CheckFail, "permission denied")
	})
	t.Run("invalid", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, nil)
		e.compose.configErr = errors.New("services.hpds.image must be a string")
		wantCheck(t, e.run(), "compose-config", ops.CheckFail, "must be a string")
	})
	t.Run("yml override ignored", func(t *testing.T) {
		e := newDoctorEnv(t)
		st := e.stack(t, nil)
		for _, f := range []string{"overrides/a.yaml", "overrides/b.yml", "overrides/.c.yml"} {
			if err := st.MkdirAll("overrides", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := st.WriteFile(f, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		c := wantCheck(t, e.run(), "overrides", ops.CheckWarn, "overrides/b.yml")
		if strings.Contains(c.Message, "a.yaml") || strings.Contains(c.Message, ".c.yml") {
			t.Errorf("message %q", c.Message)
		}
	})
}

func TestDoctorStackPorts(t *testing.T) {
	t.Run("busy elsewhere", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, nil)
		e.host.busy = map[int]bool{8443: true}
		c := wantCheck(t, e.run(), "ports", ops.CheckFail, "ports 8443 are in use by something other")
		if c.Detail == "" {
			t.Error("no detail")
		}
	})
	t.Run("owned by this stack", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, nil)
		e.host.busy = map[int]bool{8080: true, 8443: true}
		e.compose.ps = []docker.ComposeService{{Service: "httpd", Publishers: []docker.ComposePublisher{
			{PublishedPort: 8080, TargetPort: 80}, {PublishedPort: 8443, TargetPort: 443},
		}}}
		wantCheck(t, e.run(), "ports", ops.CheckOK, "ports 8080, 8443 are in use by this stack")
	})
	t.Run("dev ports", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, func(c *stack.Config) { c.Dev.Services = []string{"psama"} })
		e.host.busy = map[int]bool{15006: true}
		wantCheck(t, e.run(), "ports", ops.CheckFail, "15006")
	})
}

func TestDoctorStackAuth0(t *testing.T) {
	required := func(c *stack.Config) {
		c.Auth.Mode = stack.AuthRequired
		c.Auth.Auth0.ClientID = "abc123"
	}
	t.Run("no secret", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.stack(t, required)
		wantCheck(t, e.run(), "auth0", ops.CheckFail, "the Auth0 client secret (no secrets.yaml yet)")
	})
	t.Run("set", func(t *testing.T) {
		e := newDoctorEnv(t)
		st := e.stack(t, required)
		if err := st.SaveSecrets(&stack.Secrets{Auth0ClientSecret: "s3cret-s3cret-s3cret-s3cret-s3cret"}); err != nil {
			t.Fatal(err)
		}
		wantCheck(t, e.run(), "auth0", ops.CheckOK, "tenant avillachlab")
	})
	t.Run("empty secret", func(t *testing.T) {
		e := newDoctorEnv(t)
		st := e.stack(t, required)
		if err := st.SaveSecrets(&stack.Secrets{}); err != nil {
			t.Fatal(err)
		}
		wantCheck(t, e.run(), "auth0", ops.CheckFail, "missing: the Auth0 client secret")
	})
	t.Run("secret generated for open mode", func(t *testing.T) {
		e := newDoctorEnv(t)
		st := e.stack(t, required)
		if err := st.SaveSecrets(&stack.Secrets{Auth0ClientSecret: "s3cret-s3cret-s3cret-s3cret-s3cret", Auth0ClientSecretGenerated: true}); err != nil {
			t.Fatal(err)
		}
		wantCheck(t, e.run(), "auth0", ops.CheckFail, "only the random one made for open mode")
	})
}

func TestDoctorStackProxy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proxy stack.Proxy
		state ops.CheckStatus
		msg   string
	}{
		{"none", stack.Proxy{}, ops.CheckOK, "no proxy configured"},
		{"both", stack.Proxy{HTTP: "http://proxy:3128", HTTPS: "http://proxy:3128"}, ops.CheckOK, "proxy:3128"},
		{"http only", stack.Proxy{HTTP: "http://proxy:3128"}, ops.CheckWarn, "proxy.https is empty"},
		{"credentials", stack.Proxy{HTTP: "http://proxy:3128", HTTPS: "http://u:hunter2@proxy:3128"}, ops.CheckWarn, "credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDoctorEnv(t)
			e.stack(t, func(c *stack.Config) { c.Proxy = tc.proxy })
			c := wantCheck(t, e.run(), "proxy", tc.state, tc.msg)
			if strings.Contains(c.Message, "hunter2") {
				t.Errorf("message leaks the password: %q", c.Message)
			}
		})
	}
}

func TestDoctorNetwork(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.opts.Network = true
		e.host.reach = func(u string) (int, error) {
			if strings.Contains(u, "npmjs") {
				return 0, errors.New("dial tcp: i/o timeout")
			}
			return http.StatusMovedPermanently, nil
		}
		r := e.run()
		wantCheck(t, r, "network-github", ops.CheckOK, "https://github.com/ directly (HTTP 301)")
		wantCheck(t, r, "network-maven-central", ops.CheckOK, "")
		wantCheck(t, r, "network-npm-registry", ops.CheckFail, "i/o timeout")
		wantCheck(t, r, "network-alpine-cdn", ops.CheckOK, "")
		wantCheck(t, r, "network-release-control", ops.CheckOK, "pic-sure-baseline-release-control directly")
		noCheck(t, r, "network-docker-pull")
		if len(e.host.proxied) != 0 || len(e.git.env) != 0 {
			t.Errorf("proxied %v, git env %v", e.host.proxied, e.git.env)
		}
	})
	t.Run("proxy", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.opts.Network = true
		e.stack(t, func(c *stack.Config) {
			c.Proxy = stack.Proxy{HTTP: "http://proxy:3128", HTTPS: "http://proxy:3128"}
			c.Release.Repo = "https://git.example.org/release-control"
		})
		e.host.reach = func(u string) (int, error) {
			if strings.Contains(u, "maven") {
				// What Go's transport returns when the proxy refuses CONNECT.
				return 0, errors.New("Proxy Authentication Required")
			}
			if strings.Contains(u, "npmjs") {
				return http.StatusProxyAuthRequired, nil
			}
			return http.StatusOK, nil
		}
		e.git.err = errors.New("Could not resolve host")
		r := e.run()
		wantCheck(t, r, "network-github", ops.CheckOK, "through the proxy")
		wantCheck(t, r, "network-maven-central", ops.CheckFail, "the proxy wants credentials for https://repo.maven.apache.org/maven2/ (HTTP 407)")
		wantCheck(t, r, "network-npm-registry", ops.CheckFail, "HTTP 407")
		wantCheck(t, r, "network-release-control", ops.CheckFail, "git.example.org/release-control through the proxy: Could not resolve host")
		wantCheck(t, r, "network-docker-pull", ops.CheckOK, "pulled alpine:")
		if len(e.host.proxied) != 4 {
			t.Errorf("proxied %v, want all four", e.host.proxied)
		}
		if !strings.Contains(strings.Join(e.git.env, " "), "HTTPS_PROXY=http://proxy:3128") {
			t.Errorf("git env %v", e.git.env)
		}
	})
	t.Run("repo token redacted", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.opts.Network = true
		e.stack(t, func(c *stack.Config) { c.Release.Repo = "https://ghp_s3cret@git.example.org/rc.git" })
		e.git.err = errors.New("git ls-remote https://ghp_s3cret@git.example.org/rc.git exited 128")
		c := wantCheck(t, e.run(), "network-release-control", ops.CheckFail, "https://xxxxx@git.example.org/rc.git directly: git ls-remote https://xxxxx@")
		if strings.Contains(c.Message, "s3cret") {
			t.Errorf("message leaks the token: %q", c.Message)
		}
	})
	t.Run("http-only proxy leaves https direct", func(t *testing.T) {
		e := newDoctorEnv(t)
		e.opts.Network = true
		e.stack(t, func(c *stack.Config) { c.Proxy = stack.Proxy{HTTP: "http://proxy:3128"} })
		r := e.run()
		wantCheck(t, r, "network-github", ops.CheckOK, "https://github.com/ directly")
		wantCheck(t, r, "network-release-control", ops.CheckOK, "release-control directly")
		wantCheck(t, r, "network-docker-pull", ops.CheckOK, "")
	})
	for _, tc := range []struct {
		runtime, os, ctx, want string
	}{
		{"desktop", "Docker Desktop", "desktop-linux", "Settings > Resources > Proxies"},
		{"colima", "Ubuntu", "colima", "colima start --edit"},
		{"orbstack", "OrbStack", "orbstack", "orb config set network_proxy http://u:xxxxx@proxy:3128"},
		{"engine", "Ubuntu 24.04", "default", "HTTPS_PROXY=http://u:xxxxx@proxy:3128"},
	} {
		t.Run("pull fails on "+tc.runtime, func(t *testing.T) {
			e := newDoctorEnv(t)
			e.opts.Network = true
			e.info.OperatingSystem, e.info.ClientInfo.Context = tc.os, tc.ctx
			e.version.Server.Platform.Name = ""
			e.stack(t, func(c *stack.Config) {
				c.Proxy = stack.Proxy{HTTP: "http://u:hunter2@proxy:3128", HTTPS: "http://u:hunter2@proxy:3128"}
			})
			e.f.On(fakerunner.Glob("docker pull *")).Exit(1).Stderr("Error response from daemon: Get \"https://registry-1.docker.io/v2/\": dial tcp: i/o timeout\n")
			c := wantCheck(t, e.run(), "network-docker-pull", ops.CheckFail, "i/o timeout")
			if !strings.Contains(c.Detail, tc.want) || !strings.Contains(c.Detail, "The daemon has no proxy configured.") {
				t.Errorf("detail %q, want %q", c.Detail, tc.want)
			}
			if strings.Contains(c.Detail, "hunter2") {
				t.Errorf("detail leaks the password: %q", c.Detail)
			}
		})
	}
}

func TestDoctorReportJSON(t *testing.T) {
	r := &ops.DoctorReport{Checks: []ops.Check{{Name: "git", Status: ops.CheckOK, Message: "git is on PATH"}}}
	if got, want := jsonOf(t, r), `{"checks":[{"name":"git","status":"ok","message":"git is on PATH"}]}`; got != want {
		t.Errorf("JSON = %s\nwant   %s", got, want)
	}
}
