package docker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

func TestRunArgv(t *testing.T) {
	input := t.TempDir()
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "run", "--rm",
		"--name", "demo-hpds-load-1a2b3c4d",
		"--user", "0:0",
		"-w", "/opt/local/hpds",
		"--entrypoint", "sh",
		"--network", "demo_data",
		"--network-alias", "loader",
		"--label", "org.hms-dbmi.picsure.stack=demo",
		"-e", "HEAPSIZE", "-e", "MYSQL_PWD",
		"-v", "demo_hpds-data:/opt/local/hpds",
		"-v", input+":/opt/local/hpds_input:ro",
		img, "-c", "java -jar loader.jar",
	)).Stdout("loaded 42 rows\n").Stderr("WARN slow\n")

	var out, errOut bytes.Buffer
	code, err := e.Run(context.Background(), docker.RunOpts{
		Image:          img,
		Args:           []string{"-c", "java -jar loader.jar"},
		Name:           "demo-hpds-load-1a2b3c4d",
		Remove:         true,
		User:           "0:0",
		Workdir:        "/opt/local/hpds",
		Entrypoint:     "sh",
		Network:        "demo_data",
		NetworkAliases: []string{"loader"},
		Mounts: []docker.Mount{
			{Source: "demo_hpds-data", Target: "/opt/local/hpds"},
			{Source: input, Target: "/opt/local/hpds_input", ReadOnly: true},
		},
		Env:    []string{"HEAPSIZE=4096", "MYSQL_PWD=s3cret-value"},
		Labels: map[string]string{"org.hms-dbmi.picsure.stack": "demo"},
		Stdout: &out,
		Stderr: &errOut,
	})
	if code != 0 || err != nil {
		t.Fatalf("Run = %d, %v", code, err)
	}
	if out.String() != "loaded 42 rows\n" || errOut.String() != "WARN slow\n" {
		t.Errorf("stdout %q, stderr %q", out.String(), errOut.String())
	}
	c := f.Calls()[0]
	if !c.HasEnv("HEAPSIZE") || !c.HasEnv("MYSQL_PWD") {
		t.Errorf("Env = %v, want the values passed by environment", c.Env)
	}
}

func TestRunDetachedAndStdin(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "run", "-d", "--name", "demo-dictionaryetl", "hms-dbmi/dictionary-etl:0123456789ab")).
		Stdout("5f3a\n")
	f.On(fakerunner.Exact("docker", "run", "-i", "--rm", "-e", "MYSQL_PWD", "mysql:8.0", "mysql", "-h", "db.example.org", "-uroot"))
	ctx := context.Background()

	if code, err := e.Run(ctx, docker.RunOpts{Image: "hms-dbmi/dictionary-etl:0123456789ab", Name: "demo-dictionaryetl", Detach: true}); code != 0 || err != nil {
		t.Errorf("detached: %d, %v", code, err)
	}
	code, err := e.Run(ctx, docker.RunOpts{
		Image:  "mysql:8.0",
		Args:   []string{"mysql", "-h", "db.example.org", "-uroot"},
		Remove: true,
		Env:    []string{"MYSQL_PWD=s3cret-value"},
		Stdin:  strings.NewReader("SELECT 1;"),
	})
	if code != 0 || err != nil {
		t.Errorf("stdin: %d, %v", code, err)
	}
	if got := string(f.Calls()[1].Stdin); got != "SELECT 1;" {
		t.Errorf("stdin = %q", got)
	}
}

func TestRunExitCodes(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "run", "--rm", "alpine", "false")).Stderr("oops\n").Exit(1)
	f.On(fakerunner.Exact("docker", "run", "--rm", "hms-dbmi/missing:1")).
		Stderr("Unable to find image 'hms-dbmi/missing:1' locally\n" +
			"docker: Error response from daemon: pull access denied for hms-dbmi/missing, repository does not exist or may require 'docker login'\n" +
			"\n" +
			"Run 'docker run --help' for more information\n").
		Exit(125)
	ctx := context.Background()

	// The workload's own failure is its exit code, not an error.
	if code, err := e.Run(ctx, docker.RunOpts{Image: "alpine", Args: []string{"false"}, Remove: true}); code != 1 || err != nil {
		t.Errorf("container exit 1: %d, %v", code, err)
	}

	code, err := e.Run(ctx, docker.RunOpts{Image: "hms-dbmi/missing:1", Remove: true})
	var exitErr *docker.ExitError
	if code != 125 || !errors.As(err, &exitErr) {
		t.Fatalf("docker failure: %d, %v, want 125 and an *ExitError", code, err)
	}
	if !strings.HasSuffix(err.Error(), "exited 125: docker: Error response from daemon: pull access denied for hms-dbmi/missing, repository does not exist or may require 'docker login'") {
		t.Errorf("message = %q, want docker's error rather than its usage hint", err)
	}
}

func TestDockerFailuresAreErrors(t *testing.T) {
	// Real messages from docker 29.6.2; docker reuses the workload's exit
	// codes for them.
	const (
		missingCmd = "docker: Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: exec: \"nosuchbinary\": executable file not found in $PATH\n\nRun 'docker run --help' for more information\n"
		execNoCmd  = "OCI runtime exec failed: exec failed: unable to start container process: exec: \"nosuchbinary\": executable file not found in $PATH\n"
		stopped    = "Error response from daemon: container 0c9addfeb280 is not running\n"
		gone       = "Error response from daemon: No such container: gone\n"
	)
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "run", "alpine", "nosuchbinary")).Stderr(missingCmd).Exit(127)
	f.On(fakerunner.Exact("docker", "exec", "c", "nosuchbinary")).Stderr(execNoCmd).Exit(127)
	f.On(fakerunner.Exact("docker", "exec", "stopped", "true")).Stderr(stopped).Exit(1)
	f.On(fakerunner.Exact("docker", "start", "-a", "gone")).Stderr(gone).Exit(1)
	f.On(fakerunner.Exact("docker", "exec", "c", "sh")).Stderr("Error response from workload\n").Exit(1)
	ctx := context.Background()

	check := func(name string, code int, err error, wantCode int, wantMsg string) {
		t.Helper()
		var exitErr *docker.ExitError
		if code != wantCode || !errors.As(err, &exitErr) || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("%s = %d, %v; want %d and an *ExitError mentioning %q", name, code, err, wantCode, wantMsg)
		}
	}
	code, err := e.Run(ctx, docker.RunOpts{Image: "alpine", Args: []string{"nosuchbinary"}})
	check("Run, missing command", code, err, 127, "executable file not found")
	code, err = e.Exec(ctx, docker.ExecOpts{Container: "c", Args: []string{"nosuchbinary"}})
	check("Exec, missing command", code, err, 127, "executable file not found")
	code, err = e.Exec(ctx, docker.ExecOpts{Container: "stopped", Args: []string{"true"}})
	check("Exec, stopped container", code, err, 1, "is not running")
	code, err = e.Start(ctx, "gone", nil, nil)
	check("Start, missing container", code, err, 1, "No such container")
	if !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("Start, missing container: %v, want ErrNotFound", err)
	}

	// The workload's own stderr doesn't count, even if it looks similar.
	if code, err := e.Exec(ctx, docker.ExecOpts{Container: "c", Args: []string{"sh"}}); code != 1 || err != nil {
		t.Errorf("workload failure = %d, %v, want 1, nil", code, err)
	}
}

// concurrentRunner writes stdout and stderr from separate goroutines, as a
// real runner may.
type concurrentRunner struct{ docker.Runner }

func (concurrentRunner) Stream(_ context.Context, _ docker.Cmd, stdout, stderr io.Writer) (int, error) {
	var wg sync.WaitGroup
	for _, w := range []io.Writer{stdout, stderr} {
		wg.Go(func() {
			for range 100 {
				_, _ = w.Write([]byte("line\n"))
			}
		})
	}
	wg.Wait()
	return 0, nil
}

func TestOneWriterForBothStreams(t *testing.T) {
	e := docker.NewEngine(concurrentRunner{})
	var out bytes.Buffer
	if _, err := e.Run(context.Background(), docker.RunOpts{Image: "alpine", Stdout: &out, Stderr: &out}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 200*len("line\n") {
		t.Errorf("got %d bytes, want every write", out.Len())
	}
}

func TestRunRunnerError(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker run *")).Err(context.Canceled)
	if _, err := e.Run(context.Background(), docker.RunOpts{Image: "alpine"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestMountValidation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "allConcepts.csv")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	colonDir := filepath.Join(dir, "a:b")
	if err := os.Mkdir(colonDir, 0o700); err != nil {
		t.Fatal(err)
	}

	ok := []docker.Mount{
		{Source: dir, Target: "/in", ReadOnly: true},
		{Source: file, Target: "/opt/local/hpds/allConcepts.csv"},
		{Source: "demo_hpds-data", Target: "/opt/local/hpds"},
		{Source: "pic-sure-m2", Target: "/root/.m2"},
	}
	bad := map[string]struct {
		m    docker.Mount
		want string
	}{
		"missing bind source":  {docker.Mount{Source: filepath.Join(dir, "nope"), Target: "/in"}, "no such file"},
		"colon in bind source": {docker.Mount{Source: colonDir, Target: "/in"}, "contains ':'"},
		"relative host path":   {docker.Mount{Source: "certs/trust", Target: "/in"}, "neither an absolute host path nor a volume name"},
		"dot-relative path":    {docker.Mount{Source: "./data", Target: "/in"}, "neither an absolute host path nor a volume name"},
		"empty source":         {docker.Mount{Target: "/in"}, "neither an absolute host path nor a volume name"},
		"one-letter volume":    {docker.Mount{Source: "v", Target: "/in"}, "neither an absolute host path nor a volume name"},
		"relative target":      {docker.Mount{Source: dir, Target: "in"}, "absolute container path"},
		"colon in target":      {docker.Mount{Source: dir, Target: "/in:rw"}, "absolute container path"},
	}

	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker run *"))
	f.On(fakerunner.Glob("docker create *")).Stdout("5f3a\n")
	ctx := context.Background()
	if _, err := e.Run(ctx, docker.RunOpts{Image: "alpine", Mounts: ok}); err != nil {
		t.Errorf("valid mounts: %v", err)
	}
	for name, tt := range bad {
		_, err := e.Run(ctx, docker.RunOpts{Image: "alpine", Mounts: []docker.Mount{tt.m}})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Run, %s: err = %v, want it to mention %q", name, err, tt.want)
		}
		if _, err := e.Create(ctx, docker.RunOpts{Image: "alpine", Mounts: []docker.Mount{tt.m}}); err == nil {
			t.Errorf("Create, %s: no error", name)
		}
	}
	if n := len(f.Calls()); n != 1 {
		t.Errorf("docker ran %d times, want only for the valid mounts", n)
	}
}

func TestEnvValidation(t *testing.T) {
	_, e := newEngine(t)
	ctx := context.Background()
	for _, env := range []string{
		"MYSQL_PWD", "s3cret-value", "=s3cret-value", "1ABC=s3cret-value", "A B=s3cret-value",
		"HOME=/root", "PATH=/bin", "DOCKER_HOST=tcp://x", "XDG_RUNTIME_DIR=/run", "SSH_AUTH_SOCK=/s",
	} {
		_, err := e.Run(ctx, docker.RunOpts{Image: "alpine", Env: []string{env}})
		if err == nil {
			t.Errorf("Run with env %q: no error", env)
		} else if strings.Contains(err.Error(), "s3cret-value") {
			t.Errorf("Run with env %q: error %q shows the value", env, err)
		}
		if _, err := e.Exec(ctx, docker.ExecOpts{Container: "c", Args: []string{"true"}, Env: []string{env}}); err == nil {
			t.Errorf("Exec with env %q: no error", env)
		}
	}
}

func TestRunNeedsAnImage(t *testing.T) {
	_, e := newEngine(t)
	if _, err := e.Run(context.Background(), docker.RunOpts{Args: []string{"true"}}); err == nil {
		t.Error("no error")
	}
}

func TestCreateAndStart(t *testing.T) {
	src := t.TempDir()
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "create",
		"--name", "pic-sure-reactor-1a2b3c4d",
		"-w", "/build",
		"--entrypoint", "sh",
		"-v", src+":/src:ro",
		"-v", "pic-sure-m2:/root/.m2",
		"maven:3-amazoncorretto-25", "-c", "cp -r /src/. /build && mvn -B install -T1C -DskipTests",
	)).Stdout("5f3a9c\n")
	f.On(fakerunner.Exact("docker", "start", "-a", "5f3a9c")).Stdout("[INFO] BUILD FAILURE\n").Exit(1)
	ctx := context.Background()

	id, err := e.Create(ctx, docker.RunOpts{
		Image:      "maven:3-amazoncorretto-25",
		Args:       []string{"-c", "cp -r /src/. /build && mvn -B install -T1C -DskipTests"},
		Name:       "pic-sure-reactor-1a2b3c4d",
		Workdir:    "/build",
		Entrypoint: "sh",
		Mounts: []docker.Mount{
			{Source: src, Target: "/src", ReadOnly: true},
			{Source: "pic-sure-m2", Target: "/root/.m2"},
		},
	})
	if id != "5f3a9c" || err != nil {
		t.Fatalf("Create = %q, %v", id, err)
	}
	var out bytes.Buffer
	code, err := e.Start(ctx, id, &out, nil)
	if code != 1 || err != nil || out.String() != "[INFO] BUILD FAILURE\n" {
		t.Errorf("Start = %d, %v, output %q", code, err, out.String())
	}
}

func TestCreateRejectsRunOnlyOptions(t *testing.T) {
	_, e := newEngine(t)
	for name, opts := range map[string]docker.RunOpts{
		"Detach": {Image: "alpine", Detach: true},
		"Stdin":  {Image: "alpine", Stdin: strings.NewReader("")},
		"Stdout": {Image: "alpine", Stdout: &bytes.Buffer{}},
		"Stderr": {Image: "alpine", Stderr: &bytes.Buffer{}},
	} {
		if _, err := e.Create(context.Background(), opts); err == nil {
			t.Errorf("Create with %s: no error", name)
		}
	}
}

func TestCreateFailure(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Glob("docker create *")).
		Stderr("Error response from daemon: Conflict. The container name \"/x\" is already in use\n").Exit(1)
	_, err := e.Create(context.Background(), docker.RunOpts{Image: "alpine", Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("err = %v", err)
	}
}

func TestExec(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "exec", "-i", "--user", "mysql", "-w", "/tmp", "-e", "MYSQL_PWD", "5f3a", "mysql", "-uroot")).
		Stdout("1\n")
	f.On(fakerunner.Exact("docker", "exec", "5f3a", "test", "-s", "/data/columnMeta.csv")).Exit(1)
	ctx := context.Background()

	var out bytes.Buffer
	code, err := e.Exec(ctx, docker.ExecOpts{
		Container: "5f3a",
		Args:      []string{"mysql", "-uroot"},
		User:      "mysql",
		Workdir:   "/tmp",
		Env:       []string{"MYSQL_PWD=s3cret-value"},
		Stdin:     iotest.OneByteReader(strings.NewReader("SELECT 1;")),
		Stdout:    &out,
	})
	if code != 0 || err != nil || out.String() != "1\n" {
		t.Errorf("Exec = %d, %v, output %q", code, err, out.String())
	}
	c := f.Calls()[0]
	if string(c.Stdin) != "SELECT 1;" || !c.HasEnv("MYSQL_PWD") {
		t.Errorf("stdin %q, env %v", c.Stdin, c.Env)
	}

	if code, err := e.Exec(ctx, docker.ExecOpts{Container: "5f3a", Args: []string{"test", "-s", "/data/columnMeta.csv"}}); code != 1 || err != nil {
		t.Errorf("exit 1: %d, %v", code, err)
	}
	if _, err := e.Exec(ctx, docker.ExecOpts{Container: "5f3a"}); err == nil {
		t.Error("no command: no error")
	}
}

func TestCpFrom(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "cp", "5f3a9c:/build/services/hpds/.", "/cache/build/0123456789ab/services/hpds"))
	f.On(fakerunner.Exact("docker", "cp", "5f3a9c:/missing", "/tmp/x")).
		Stderr("Error response from daemon: Could not find the file /missing in container 5f3a9c\n").Exit(1)
	ctx := context.Background()

	if err := e.CpFrom(ctx, "5f3a9c", "/build/services/hpds/.", "/cache/build/0123456789ab/services/hpds"); err != nil {
		t.Fatal(err)
	}
	if err := e.CpFrom(ctx, "5f3a9c", "/missing", "/tmp/x"); err == nil || !strings.Contains(err.Error(), "Could not find the file") {
		t.Errorf("missing path: %v", err)
	}
	if err := e.CpFrom(ctx, "5f3a9c", "build", "/tmp/x"); err == nil {
		t.Error("relative container path: no error")
	}
	if err := e.CpFrom(ctx, "5f3a9c", "/build", "out"); err == nil {
		t.Error("relative host path: no error")
	}
}

func TestRm(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "rm", "-v", "5f3a9c"))
	f.On(fakerunner.Exact("docker", "rm", "-v", "-f", "demo-dictionaryetl"))
	f.On(fakerunner.Exact("docker", "rm", "-v", "gone")).Stderr("Error response from daemon: No such container: gone\n").Exit(1)
	f.On(fakerunner.Exact("docker", "rm", "-v", "busy")).
		Stderr("Error response from daemon: cannot remove container \"/busy\": container is running: stop the container before removing or force remove\n").Exit(1)
	ctx := context.Background()

	if err := e.Rm(ctx, "5f3a9c", false); err != nil {
		t.Error(err)
	}
	if err := e.Rm(ctx, "demo-dictionaryetl", true); err != nil {
		t.Error(err)
	}
	if err := e.Rm(ctx, "gone", false); err != nil {
		t.Errorf("already gone: %v, want nil", err)
	}
	if err := e.Rm(ctx, "busy", false); err == nil {
		t.Error("running container: no error")
	}
}

func TestContainerInspect(t *testing.T) {
	f, e := newEngine(t)
	f.On(fakerunner.Exact("docker", "container", "inspect", "demo-hpds-1")).Stdout(`[{
		"Id": "9a8b7c6d5e4f",
		"Name": "/demo-hpds-1",
		"Image": "sha256:33bee74c",
		"Config": {"Image": "hms-dbmi/pic-sure-hpds:0123456789ab", "Labels": {"com.docker.compose.service": "hpds"}},
		"State": {"Status": "running", "Running": true, "ExitCode": 0, "Health": {"Status": "unhealthy", "FailingStreak": 3}}
	}]`)
	f.On(fakerunner.Exact("docker", "container", "inspect", "c016")).
		Stdout(`[{"Id":"c167","Name":"/c016","Image":"sha256:1","Config":{"Image":"alpine","Labels":{}},"State":{"Status":"exited","Running":false,"ExitCode":3}}]`)
	f.On(fakerunner.Exact("docker", "container", "inspect", "gone")).Stdout("[]\n").
		Stderr("Error response from daemon: No such container: gone\n").Exit(1)
	ctx := context.Background()

	c, err := e.ContainerInspect(ctx, "demo-hpds-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "9a8b7c6d5e4f" || c.Name != "demo-hpds-1" || c.Image != "hms-dbmi/pic-sure-hpds:0123456789ab" || c.ImageID != "sha256:33bee74c" ||
		c.Status != "running" || !c.Running || c.Health != "unhealthy" ||
		!maps.Equal(c.Labels, map[string]string{"com.docker.compose.service": "hpds"}) {
		t.Errorf("ContainerInspect = %+v", c)
	}

	c, err = e.ContainerInspect(ctx, "c016")
	if err != nil || c.Health != "" || c.ExitCode != 3 || c.Status != "exited" {
		t.Errorf("no healthcheck: %+v, %v", c, err)
	}

	if _, err := e.ContainerInspect(ctx, "gone"); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("missing: %v, want ErrNotFound", err)
	}
}

func TestUniqueName(t *testing.T) {
	name, err := docker.UniqueName("demo-hpds-load", bytes.NewReader([]byte{0x1a, 0x2b, 0x3c, 0x4d}))
	if name != "demo-hpds-load-1a2b3c4d" || err != nil {
		t.Errorf("UniqueName = %q, %v", name, err)
	}
	if _, err := docker.UniqueName("x", bytes.NewReader([]byte{1})); err == nil {
		t.Error("short read: no error")
	}
}
