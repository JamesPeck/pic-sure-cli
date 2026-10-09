package docker_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/docker/fakerunner"
)

const secretValue = "s3cret-value-never-in-argv"

func newTestCompose(f *fakerunner.Runner) *docker.Compose {
	return &docker.Compose{
		Runner:     f,
		Files:      []string{"/stack/.pic-sure/render/compose.yaml", "/stack/overrides/a.yaml"},
		ProjectDir: "/stack",
		Project:    "demo",
		Env: func() []string {
			return []string{"PICSURE_DB_PASSWORD=" + secretValue, "HPDS_TAG=abc123"}
		},
	}
}

// composeArgv is the argv newTestCompose gives a verb: the global flags,
// --progress plain if progress, then args.
func composeArgv(progress bool, args ...string) []string {
	argv := []string{
		"docker", "compose",
		"-f", "/stack/.pic-sure/render/compose.yaml",
		"-f", "/stack/overrides/a.yaml",
		"--project-directory", "/stack",
		"--env-file", os.DevNull,
		"-p", "demo",
	}
	if progress {
		argv = append(argv, "--progress", "plain")
	}
	return append(argv, args...)
}

func TestComposeVerbArgv(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func(c *docker.Compose) error
		want []string
	}{
		{"up all", func(c *docker.Compose) error {
			return c.Up(ctx, docker.ComposeUpOpts{})
		}, composeArgv(true, "up", "-d")},
		{"up and wait for services", func(c *docker.Compose) error {
			return c.Up(ctx, docker.ComposeUpOpts{Services: []string{"picsure-db", "hpds"}, Wait: true})
		}, composeArgv(true, "up", "-d", "--wait", "picsure-db", "hpds")},
		{"wait timeout rounds up to whole seconds", func(c *docker.Compose) error {
			return c.Up(ctx, docker.ComposeUpOpts{Wait: true, WaitTimeout: 90*time.Second + time.Millisecond})
		}, composeArgv(true, "up", "-d", "--wait", "--wait-timeout", "91")},
		{"wait timeout ignored without wait", func(c *docker.Compose) error {
			return c.Up(ctx, docker.ComposeUpOpts{WaitTimeout: time.Minute})
		}, composeArgv(true, "up", "-d")},
		{"down", func(c *docker.Compose) error {
			return c.Down(ctx, docker.ComposeDownOpts{})
		}, composeArgv(true, "down", "--remove-orphans")},
		{"down with volumes", func(c *docker.Compose) error {
			return c.Down(ctx, docker.ComposeDownOpts{Volumes: true})
		}, composeArgv(true, "down", "--remove-orphans", "--volumes")},
		{"stop", func(c *docker.Compose) error {
			return c.Stop(ctx, nil, "hpds")
		}, composeArgv(true, "stop", "hpds")},
		{"restart all", func(c *docker.Compose) error {
			return c.Restart(ctx, nil)
		}, composeArgv(true, "restart")},
		{"restart services", func(c *docker.Compose) error {
			return c.Restart(ctx, nil, "psama", "httpd")
		}, composeArgv(true, "restart", "psama", "httpd")},
		{"pull", func(c *docker.Compose) error {
			return c.Pull(ctx, nil, "httpd")
		}, composeArgv(true, "pull", "httpd")},
		{"run with rm", func(c *docker.Compose) error {
			_, err := c.Run(ctx, docker.ComposeRunOpts{Service: "flyway-init", Rm: true})
			return err
		}, composeArgv(true, "run", "--rm", "-T", "flyway-init")},
		{"run with a command", func(c *docker.Compose) error {
			_, err := c.Run(ctx, docker.ComposeRunOpts{Service: "hpds-etl", Args: []string{"java", "-jar", "--flag"}})
			return err
		}, composeArgv(true, "run", "-T", "hpds-etl", "java", "-jar", "--flag")},
		{"exec", func(c *docker.Compose) error {
			_, err := c.Exec(ctx, docker.ComposeExecOpts{Service: "gateway", Args: []string{"wget", "-qO-", "http://localhost/"}})
			return err
		}, composeArgv(false, "exec", "-T", "gateway", "wget", "-qO-", "http://localhost/")},
		{"logs all", func(c *docker.Compose) error {
			return c.Logs(ctx, docker.ComposeLogsOpts{})
		}, composeArgv(false, "logs")},
		{"logs follow with tail", func(c *docker.Compose) error {
			return c.Logs(ctx, docker.ComposeLogsOpts{Follow: true, Tail: 500, Services: []string{"hpds"}})
		}, composeArgv(false, "logs", "--follow", "--tail", "500", "hpds")},
		{"ps", func(c *docker.Compose) error {
			_, err := c.Ps(ctx)
			return err
		}, composeArgv(false, "ps", "--all", "--format", "json")},
		{"ps services", func(c *docker.Compose) error {
			_, err := c.Ps(ctx, "picsure-db")
			return err
		}, composeArgv(false, "ps", "--all", "--format", "json", "picsure-db")},
		{"config quiet", func(c *docker.Compose) error {
			_, err := c.Config(ctx, true)
			return err
		}, composeArgv(false, "config", "--quiet")},
		{"config", func(c *docker.Compose) error {
			_, err := c.Config(ctx, false)
			return err
		}, composeArgv(false, "config", "--no-interpolate")},
		{"passthrough", func(c *docker.Compose) error {
			_, err := c.Passthrough(ctx, []string{"exec", "hpds", "sh"}, nil, nil, nil)
			return err
		}, composeArgv(false, "exec", "hpds", "sh")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fakerunner.New(t)
			f.On(fakerunner.Exact(tt.want...))
			if err := tt.call(newTestCompose(f)); err != nil {
				t.Fatal(err)
			}
			calls := f.Calls()
			if len(calls) != 1 {
				t.Fatalf("got %d calls, want 1", len(calls))
			}
			c := calls[0]
			if want := []string{"PICSURE_DB_PASSWORD", "HPDS_TAG"}; !slices.Equal(c.Env, want) {
				t.Errorf("env names = %v, want %v", c.Env, want)
			}
			if c.Dir != "/stack" {
				t.Errorf("dir = %q, want /stack", c.Dir)
			}
			if strings.Contains(strings.Join(c.Argv, " "), secretValue) {
				t.Errorf("secret value in argv: %s", c)
			}
		})
	}
}

func TestComposeProgressFormat(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * --progress json up -d"))
	f.On(fakerunner.Glob("docker compose * logs"))
	f.On(fakerunner.Glob("docker compose * version"))
	c := newTestCompose(f)
	c.Progress = docker.ProgressJSON
	ctx := context.Background()

	if err := c.Up(ctx, docker.ComposeUpOpts{}); err != nil {
		t.Fatal(err)
	}
	// Only verbs that report progress get --progress, and Passthrough adds
	// nothing to the user's arguments.
	if err := c.Logs(ctx, docker.ComposeLogsOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Passthrough(ctx, []string{"version"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.AssertNotCalled(fakerunner.Glob("* --progress * logs"))
	f.AssertNotCalled(fakerunner.Glob("* --progress * version"))
}

func TestComposeStreamsOutput(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * up -d --wait")).
		Stdout("out line\n").Stderr(" Container demo-hpds-1  Healthy\n")
	var out bytes.Buffer
	if err := newTestCompose(f).Up(context.Background(), docker.ComposeUpOpts{Wait: true, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "out line\n") || !strings.Contains(got, " Container demo-hpds-1  Healthy\n") {
		t.Errorf("out = %q, want both streams", got)
	}
}

func TestComposeLogsSplitsComposesOwnMessages(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * logs nosuch")).
		Stdout("hpds-1  | started\n").Stderr("no such service: nosuch\n").Exit(1)
	var out, errOut bytes.Buffer
	err := newTestCompose(f).Logs(context.Background(), docker.ComposeLogsOpts{Services: []string{"nosuch"}, Out: &out, Err: &errOut})
	if err == nil || !strings.Contains(err.Error(), "no such service: nosuch") {
		t.Errorf("err = %v, want compose's message", err)
	}
	if out.String() != "hpds-1  | started\n" || errOut.String() != "no such service: nosuch\n" {
		t.Errorf("out = %q, err = %q, want the logs and compose's message apart", out.String(), errOut.String())
	}
}

func TestComposeVerbFailureCarriesStderr(t *testing.T) {
	for _, withWriter := range []bool{false, true} {
		f := fakerunner.New(t)
		f.On(fakerunner.Glob("docker compose * up -d --wait")).
			Stderr(" Container demo-picsure-db-1  Error\ndependency failed to start: container demo-picsure-db-1 is unhealthy\n").
			Exit(1)
		var buf bytes.Buffer
		opts := docker.ComposeUpOpts{Wait: true}
		if withWriter {
			opts.Out = &buf
		}
		err := newTestCompose(f).Up(context.Background(), opts)
		var exitErr *docker.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode != 1 {
			t.Fatalf("writer %v: err = %v, want an *ExitError with code 1", withWriter, err)
		}
		if !strings.HasSuffix(err.Error(), ": dependency failed to start: container demo-picsure-db-1 is unhealthy") {
			t.Errorf("writer %v: message %q lacks the last stderr line", withWriter, err)
		}
		if withWriter && !strings.Contains(buf.String(), "is unhealthy") {
			t.Errorf("writer got %q", buf.String())
		}
	}
}

func TestComposeFailureKeepsOnlyTheStderrTail(t *testing.T) {
	f := fakerunner.New(t)
	long := strings.Repeat("progress line\n", 10_000) + "Error: the real cause\n" +
		"Run 'docker compose pull --help' for more information\n"
	f.On(fakerunner.Glob("docker compose * pull")).Stderr(long).Exit(18)
	err := newTestCompose(f).Pull(context.Background(), nil)
	var exitErr *docker.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want an *ExitError", err)
	}
	if n := len(exitErr.Stderr); n == 0 || n > 4096 {
		t.Errorf("kept %d bytes of stderr, want 1..4096", n)
	}
	if !strings.HasSuffix(err.Error(), ": Error: the real cause") {
		t.Errorf("message = %q, want the cause without the usage hint", err)
	}
}

func TestComposeRunReturnsExitCode(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * run --rm -T flyway-init")).
		Stdout("Successfully applied 3 migrations\n").Stderr("ERROR: Validate failed\n").Exit(1)
	var stdout, stderr bytes.Buffer
	code, err := newTestCompose(f).Run(context.Background(), docker.ComposeRunOpts{
		Service: "flyway-init",
		Rm:      true,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil || code != 1 {
		t.Fatalf("code %d, err %v; want 1 and no error", code, err)
	}
	if stdout.String() != "Successfully applied 3 migrations\n" || stderr.String() != "ERROR: Validate failed\n" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestComposeRunReportsDockerFailures(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * run --rm -T flyway-init")).
		Stderr("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n").Exit(1)
	code, err := newTestCompose(f).Run(context.Background(), docker.ComposeRunOpts{Service: "flyway-init", Rm: true})
	var exitErr *docker.ExitError
	if code != 1 || !errors.As(err, &exitErr) {
		t.Errorf("code %d, err %v; want 1 and an *ExitError", code, err)
	}
}

func TestComposeOwnFailuresAreErrors(t *testing.T) {
	failures := []struct {
		name    string
		stderr  string
		message string
	}{
		{"no such service", "no such service: nosuch\n", "no such service: nosuch"},
		{"service not running", "service \"flyway-init\" is not running\n", "service \"flyway-init\" is not running"},
		{"interpolation",
			"error while interpolating services.hpds.image: required variable HPDS_TAG is missing a value\n",
			"error while interpolating services.hpds.image: required variable HPDS_TAG is missing a value"},
		{"unhealthy dependency",
			" Container demo-picsure-db-1  Error\ndependency failed to start: container demo-picsure-db-1 is unhealthy\n",
			"dependency failed to start: container demo-picsure-db-1 is unhealthy"},
		{"failed one-shot dependency",
			"service \"flyway-init\" didn't complete successfully: exit 3\n",
			"service \"flyway-init\" didn't complete successfully: exit 3"},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			f := fakerunner.New(t)
			f.On(fakerunner.Glob("docker compose *")).Stderr(tt.stderr).Exit(1)
			c := newTestCompose(f)
			ctx := context.Background()

			_, runErr := c.Run(ctx, docker.ComposeRunOpts{Service: "hpds", Rm: true})
			_, execErr := c.Exec(ctx, docker.ComposeExecOpts{Service: "hpds", Args: []string{"true"}})
			upErr := c.Up(ctx, docker.ComposeUpOpts{})
			assertComposeError(t, map[string]error{"run": runErr, "exec": execErr, "up": upErr}, tt.message)
		})
	}
}

func assertComposeError(t *testing.T, errs map[string]error, message string) {
	t.Helper()
	for verb, err := range errs {
		var exitErr *docker.ExitError
		if !errors.As(err, &exitErr) {
			t.Errorf("%s: err = %v, want an *ExitError", verb, err)
		} else if !strings.HasSuffix(err.Error(), "exited "+strconv.Itoa(exitErr.ExitCode)+": "+message) {
			t.Errorf("%s: message %q, want it to end with %q", verb, err, message)
		}
	}
}

// Under --progress json compose ends stderr with {"error":true} after any
// failure, and adds a message when the failure was its own.
func TestComposeJSONProgressErrors(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("* run --rm -T flyway-init")).
		Stderr(`{"id":"Container demo-picsure-db-1","status":"Error"}` + "\n" + `{"error":true,"message":"no such service: flyway-init"}` + "\n").Exit(1)
	f.On(fakerunner.Glob("* run --rm -T hpds-etl")).Stderr("ERROR: input file missing\n{\"error\":true}\n").Exit(3)
	f.On(fakerunner.Glob("* up -d")).Stderr("{\"error\":true,\"message\":\"dependency failed to start: container demo-picsure-db-1 is unhealthy\"}\n").Exit(1)
	c := newTestCompose(f)
	c.Progress = docker.ProgressJSON
	ctx := context.Background()

	_, runErr := c.Run(ctx, docker.ComposeRunOpts{Service: "flyway-init", Rm: true})
	assertComposeError(t, map[string]error{"run": runErr}, "no such service: flyway-init")
	upErr := c.Up(ctx, docker.ComposeUpOpts{})
	assertComposeError(t, map[string]error{"up": upErr}, "dependency failed to start: container demo-picsure-db-1 is unhealthy")

	var stderr bytes.Buffer
	code, err := c.Run(ctx, docker.ComposeRunOpts{Service: "hpds-etl", Rm: true, Stderr: &stderr})
	if code != 3 || err != nil {
		t.Errorf("workload failure: code %d, err %v; want 3 and no error", code, err)
	}
	if !strings.Contains(stderr.String(), "ERROR: input file missing") {
		t.Errorf("stderr writer got %q", stderr.String())
	}
}

func TestComposeWorkloadJSONIsTheWorkloadsOwn(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose *")).Stderr(`{"error":true,"message":"no such service: x"}` + "\n").Exit(2)
	c := newTestCompose(f)
	c.Progress = docker.ProgressJSON // exec never passes --progress
	code, err := c.Exec(context.Background(), docker.ComposeExecOpts{Service: "api", Args: []string{"validate"}})
	if code != 2 || err != nil {
		t.Errorf("exec: code %d, err %v; want 2 and no error", code, err)
	}
	c.Progress = docker.ProgressPlain
	code, err = c.Run(context.Background(), docker.ComposeRunOpts{Service: "api"})
	if code != 2 || err != nil {
		t.Errorf("run in plain mode: code %d, err %v; want 2 and no error", code, err)
	}
}

func TestComposeRefusesEnvDockerReadsItself(t *testing.T) {
	for _, entry := range []string{"DOCKER_HOST=tcp://elsewhere:2375", "COMPOSE_PROJECT_NAME=other", "HOME=/tmp", secretValue} {
		f := fakerunner.New(t)
		c := newTestCompose(f)
		c.Env = func() []string { return []string{"HPDS_TAG=abc123", entry} }
		err := c.Stop(context.Background(), nil)
		if err == nil {
			t.Errorf("%s: no error", entry)
		} else if strings.Contains(err.Error(), secretValue) {
			t.Errorf("error leaks the entry's value: %v", err)
		}
		if n := len(f.Calls()); n != 0 {
			t.Errorf("%s: %d calls reached the runner", entry, n)
		}
	}
}

func TestComposeExec(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * exec -T picsure-db mysql")).Stdout("1\n").Stderr("warning\n").Exit(2)
	var stdout, stderr bytes.Buffer
	code, err := newTestCompose(f).Exec(context.Background(), docker.ComposeExecOpts{
		Service: "picsure-db",
		Args:    []string{"mysql"},
		Stdin:   strings.NewReader("SELECT 1;"),
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil || code != 2 {
		t.Fatalf("code %d, err %v; want 2 and no error", code, err)
	}
	if stdout.String() != "1\n" || stderr.String() != "warning\n" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if got := string(f.Calls()[0].Stdin); got != "SELECT 1;" {
		t.Errorf("stdin = %q", got)
	}
}

func TestComposeRunAndExecNeedAService(t *testing.T) {
	f := fakerunner.New(t)
	c := newTestCompose(f)
	ctx := context.Background()
	if _, err := c.Run(ctx, docker.ComposeRunOpts{}); err == nil {
		t.Error("run without a service: no error")
	}
	if _, err := c.Exec(ctx, docker.ComposeExecOpts{Service: "hpds"}); err == nil {
		t.Error("exec without a command: no error")
	}
	if n := len(f.Calls()); n != 0 {
		t.Errorf("%d calls reached the runner", n)
	}
}

func TestComposePassthrough(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Exact(composeArgv(false, "--dry-run", "up", "-d")...)).Stdout("dry run\n").Exit(3)
	var stdout bytes.Buffer
	code, err := newTestCompose(f).Passthrough(context.Background(),
		[]string{"--dry-run", "up", "-d"}, strings.NewReader("input"), &stdout, nil)
	if err != nil || code != 3 {
		t.Fatalf("code %d, err %v; want 3 and no error", code, err)
	}
	if stdout.String() != "dry run\n" || string(f.Calls()[0].Stdin) != "input" {
		t.Errorf("stdout %q, stdin %q", stdout.String(), f.Calls()[0].Stdin)
	}
}

func TestComposeRunnerErrorsPropagate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := fakerunner.New(t)
	c := newTestCompose(f)
	if err := c.Logs(ctx, docker.ComposeLogsOpts{Follow: true}); !errors.Is(err, context.Canceled) {
		t.Errorf("logs: err = %v, want context.Canceled", err)
	}
	if _, err := c.Run(ctx, docker.ComposeRunOpts{Service: "x"}); !errors.Is(err, context.Canceled) {
		t.Errorf("run: err = %v, want context.Canceled", err)
	}
	if _, err := c.Ps(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ps: err = %v, want context.Canceled", err)
	}
}

func TestComposeCallsEnvPerCall(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose *"))
	n := 0
	c := newTestCompose(f)
	c.Env = func() []string { n++; return []string{"N=x"} }
	ctx := context.Background()
	_ = c.Stop(ctx, nil)
	_ = c.Restart(ctx, nil)
	if n != 2 {
		t.Errorf("Env called %d times for 2 calls", n)
	}

	c.Env = nil
	_ = c.Stop(ctx, nil)
	if calls := f.Calls(); len(calls[2].Env) != 0 {
		t.Errorf("nil Env gave env %v", calls[2].Env)
	}
}

func TestComposeWithoutFilesRefusesToRun(t *testing.T) {
	f := fakerunner.New(t)
	c := &docker.Compose{Runner: f, ProjectDir: "/stack"}
	ctx := context.Background()
	errs := []error{
		c.Up(ctx, docker.ComposeUpOpts{}),
		c.Down(ctx, docker.ComposeDownOpts{}),
		c.Logs(ctx, docker.ComposeLogsOpts{}),
	}
	_, err := c.Run(ctx, docker.ComposeRunOpts{Service: "x"})
	errs = append(errs, err)
	_, err = c.Exec(ctx, docker.ComposeExecOpts{Service: "x", Args: []string{"true"}})
	errs = append(errs, err)
	_, err = c.Ps(ctx)
	errs = append(errs, err)
	_, err = c.Config(ctx, true)
	errs = append(errs, err)
	_, err = c.Passthrough(ctx, []string{"ps"}, nil, nil, nil)
	errs = append(errs, err)
	for i, err := range errs {
		if err == nil {
			t.Errorf("call %d: no error", i)
		}
	}
	if n := len(f.Calls()); n != 0 {
		t.Errorf("%d calls reached the runner", n)
	}
}

func TestComposePs(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * ps --all --format json")).Do(
		func(ctx context.Context, _ fakerunner.Call) (docker.Result, error) {
			dl, ok := ctx.Deadline()
			if !ok || time.Until(dl) > docker.PsTimeout {
				t.Errorf("ps deadline %v (set %v), want within %s", dl, ok, docker.PsTimeout)
			}
			return docker.Result{Stdout: []byte(
				`{"Service":"hpds","State":"running","Health":"starting","ExitCode":0}` + "\n" +
					`{"Service":"picsure-db","State":"exited","Health":null,"ExitCode":137}` + "\n",
			)}, nil
		})
	got, err := newTestCompose(f).Ps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Health != "starting" || got[1].State != "exited" || got[1].ExitCode != 137 {
		t.Errorf("services = %+v", got)
	}
}

func TestComposePsFailures(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * ps --all --format json")).Stderr("Cannot connect to the Docker daemon\n").Exit(1).Times(1)
	f.On(fakerunner.Glob("docker compose * ps --all --format json")).Err(context.DeadlineExceeded).Times(1)
	f.On(fakerunner.Glob("docker compose * ps --all --format json")).Stdout("not json\n")
	c := newTestCompose(f)
	ctx := context.Background()

	var exitErr *docker.ExitError
	if _, err := c.Ps(ctx); !errors.As(err, &exitErr) {
		t.Errorf("non-zero exit: err = %v, want an *ExitError", err)
	}
	if _, err := c.Ps(ctx); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "within 10s") {
		t.Errorf("timeout: err = %v", err)
	}
	if _, err := c.Ps(ctx); err == nil {
		t.Error("garbage output: no error")
	}
}

func TestComposeConfig(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Glob("docker compose * config --no-interpolate")).Stdout("name: demo\n")
	f.On(fakerunner.Glob("docker compose * config --quiet")).
		Stderr("error while interpolating services.hpds.image: required variable HPDS_TAG is missing a value\n").Exit(15)
	c := newTestCompose(f)

	out, err := c.Config(context.Background(), false)
	if err != nil || string(out) != "name: demo\n" {
		t.Errorf("config: %q, %v", out, err)
	}
	out, err = c.Config(context.Background(), true)
	if out != nil || err == nil || !strings.HasSuffix(err.Error(), "required variable HPDS_TAG is missing a value") {
		t.Errorf("config --quiet: %q, %v", out, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewComposeFiles(t *testing.T) {
	dir := t.TempDir()
	rendered := filepath.Join(dir, ".pic-sure", "render", "compose.yaml")
	writeFile(t, rendered, "name: demo\n")
	for _, name := range []string{"b.yaml", "a.yaml", "10-z.yaml", ".hidden.yaml", "c.yml", "notes.txt"} {
		writeFile(t, filepath.Join(dir, "overrides", name), "{}\n")
	}
	if err := os.Mkdir(filepath.Join(dir, "overrides", "dir.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := func() []string { return []string{"A=1"} }

	c, err := docker.NewCompose(fakerunner.New(t), dir, env)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		rendered,
		filepath.Join(dir, "overrides", "10-z.yaml"),
		filepath.Join(dir, "overrides", "a.yaml"),
		filepath.Join(dir, "overrides", "b.yaml"),
	}
	if !slices.Equal(c.Files, want) {
		t.Errorf("files = %q\nwant %q", c.Files, want)
	}
	if c.ProjectDir != dir || c.Project != "demo" || c.Env == nil || c.Runner == nil {
		t.Errorf("compose = %+v", c)
	}
}

func TestNewComposeWithoutOverrides(t *testing.T) {
	dir := t.TempDir()
	rendered := filepath.Join(dir, ".pic-sure", "render", "compose.yaml")
	writeFile(t, rendered, "name: demo\n")
	t.Chdir(dir)

	c, err := docker.NewCompose(fakerunner.New(t), ".", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.ProjectDir) || len(c.Files) != 1 || !filepath.IsAbs(c.Files[0]) {
		t.Errorf("compose = %+v, want one absolute file and an absolute project dir", c)
	}
	if !sameFile(t, c.Files[0], rendered) {
		t.Errorf("files[0] = %q, want %q", c.Files[0], rendered)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(fa, fb)
}

func TestNewComposeErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := docker.NewCompose(fakerunner.New(t), dir, nil); !errors.Is(err, docker.ErrNotRendered) {
		t.Errorf("unrendered stack: err = %v, want ErrNotRendered", err)
	}

	writeFile(t, filepath.Join(dir, ".pic-sure", "render", "compose.yaml"), "name: demo\n")
	writeFile(t, filepath.Join(dir, "overrides"), "not a directory\n")
	if _, err := docker.NewCompose(fakerunner.New(t), dir, nil); err == nil || errors.Is(err, docker.ErrNotRendered) {
		t.Errorf("overrides is a file: err = %v", err)
	}
}

// TestComposeAgainstRealCompose checks the global flags against the real
// compose CLI. `config` needs no daemon, only docker and its compose plugin.
func TestComposeAgainstRealCompose(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skipf("docker compose is not available: %v", err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".pic-sure", "render", "compose.yaml"), `name: picsure-test
services:
  web:
    image: busybox:${TAG:?TAG is required}
    environment:
      SHARED: base
    volumes:
      - ./data:/data
`)
	writeFile(t, filepath.Join(dir, "overrides", "b.yaml"), "services:\n  web:\n    environment:\n      SHARED: b\n")
	writeFile(t, filepath.Join(dir, "overrides", "a.yaml"), "services:\n  web:\n    environment:\n      SHARED: a\n      FROM_A: a\n")
	// An override's name: doesn't switch the project.
	writeFile(t, filepath.Join(dir, "overrides", "c.yaml"), "name: renamed\n")
	// A stray .env in the stack directory must not change the project or
	// supply values.
	writeFile(t, filepath.Join(dir, ".env"), "COMPOSE_PROJECT_NAME=hijacked\nTAG=from-dotenv\n")
	ctx := context.Background()

	c, err := docker.NewCompose(&docker.ExecRunner{}, dir, func() []string { return []string{"TAG=from-env"} })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Config(ctx, true); err != nil {
		t.Fatalf("config --quiet with TAG in Env: %v", err)
	}
	out, err := c.Config(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	model := string(out)
	for _, want := range []string{"name: picsure-test", "SHARED=b", "FROM_A=a", "${TAG:?TAG is required}", filepath.Join(dir, "data")} {
		if !strings.Contains(model, want) {
			t.Errorf("merged model lacks %q:\n%s", want, model)
		}
	}
	for _, unwanted := range []string{"hijacked", "renamed", "from-env", "from-dotenv"} {
		if strings.Contains(model, unwanted) {
			t.Errorf("merged model contains %q:\n%s", unwanted, model)
		}
	}

	c.Env = nil
	if _, err := c.Config(ctx, true); err == nil || !strings.Contains(err.Error(), "TAG is required") {
		t.Errorf("config --quiet without TAG: err = %v, want the required-variable error (.env must be ignored)", err)
	}
}

func TestComposeRunPassesEnvByName(t *testing.T) {
	f := fakerunner.New(t)
	f.On(fakerunner.Exact(composeArgv(true, "run", "--rm", "-T", "-e", "FLYWAY_ACTION", "flyway-init")...))
	c := newTestCompose(f)
	if _, err := c.Run(context.Background(), docker.ComposeRunOpts{
		Service: "flyway-init", Rm: true, Env: []string{"FLYWAY_ACTION=repair"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls()[0].Env; !slices.Contains(got, "FLYWAY_ACTION") || !slices.Contains(got, "PICSURE_DB_PASSWORD") {
		t.Errorf("env names %v, want FLYWAY_ACTION beside the stack's", got)
	}

	for _, env := range [][]string{{"HPDS_TAG=x"}, {"COMPOSE_PROJECT_NAME=x"}, {"DOCKER_HOST=x"}, {"bad"}} {
		if _, err := c.Run(context.Background(), docker.ComposeRunOpts{Service: "flyway-init", Env: env}); err == nil {
			t.Errorf("env %v: no error", env)
		}
	}
	if n := len(f.Calls()); n != 1 {
		t.Errorf("%d calls, want refused envs not to run", n)
	}
}

func TestComposeConfigHashes(t *testing.T) {
	f := fakerunner.New(t)
	var calls []fakerunner.Call
	f.On(fakerunner.Glob("docker compose * config --hash *")).Do(func(_ context.Context, c fakerunner.Call) (docker.Result, error) {
		calls = append(calls, c)
		return docker.Result{Stdout: []byte("httpd 2213c4e3\npsama 9e7ae5ea\n\n")}, nil
	})
	c := newTestCompose(f)
	got, err := c.ConfigHashes(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"httpd": "2213c4e3", "psama": "9e7ae5ea"}; !maps.Equal(got, want) {
		t.Errorf("hashes %v, want %v", got, want)
	}
	// Another render keeps the overrides and the env.
	if _, err := c.ConfigHashes(context.Background(), "/tmp/next.yaml"); err != nil {
		t.Fatal(err)
	}
	want := "docker compose -f /tmp/next.yaml -f /stack/overrides/a.yaml --project-directory /stack"
	if argv := strings.Join(calls[1].Argv, " "); !strings.HasPrefix(argv, want) || !slices.Equal(calls[1].Env, []string{"PICSURE_DB_PASSWORD", "HPDS_TAG"}) {
		t.Errorf("call %s with env %v, want %s... with the adapter's env", argv, calls[1].Env, want)
	}
	if c.Files[0] != "/stack/.pic-sure/render/compose.yaml" {
		t.Errorf("the adapter's files changed: %v", c.Files)
	}
}
