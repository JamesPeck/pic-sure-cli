package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Composer runs `docker compose` against one rendered stack: it owns the -f
// list and the per-call environment (spec §6.4), so callers never assemble
// compose argv themselves. ops.Deps.Compose holds one for the stack a command
// acts on; Compose is the implementation.
//
// Up, Down, Stop, Restart, Pull and Logs copy compose's output to a writer
// as it arrives (nil discards it) and return an *ExitError carrying
// compose's message when it exits non-zero. Run and Exec return the exit
// code of the command they run, plus an *ExitError when docker itself failed,
// as Engine.Run does. Passthrough returns compose's exit code. Every method
// returns an error when docker could not be started or ctx ended first.
type Composer interface {
	// Up creates and starts containers in the background (`up -d`).
	Up(ctx context.Context, opts ComposeUpOpts) error
	// Down stops and removes the stack's containers and networks, including
	// containers of services no longer in the compose files.
	Down(ctx context.Context, opts ComposeDownOpts) error
	// Stop stops the services' containers without removing them. No
	// services means all of them.
	Stop(ctx context.Context, out io.Writer, services ...string) error
	// Restart restarts the services. No services means all of them.
	Restart(ctx context.Context, out io.Writer, services ...string) error
	// Pull pulls the services' images. No services means all of them.
	Pull(ctx context.Context, out io.Writer, services ...string) error
	// Run runs a one-off container for a service, starting its
	// dependencies first, and returns the container's exit code.
	Run(ctx context.Context, opts ComposeRunOpts) (int, error)
	// Exec runs a command in a service's running container and returns its
	// exit code.
	Exec(ctx context.Context, opts ComposeExecOpts) (int, error)
	// Logs copies the services' logs to opts.Out; with Follow, until ctx
	// ends.
	Logs(ctx context.Context, opts ComposeLogsOpts) error
	// Ps lists the services' containers, stopped ones included. No
	// services means all of them. It gives up after PsTimeout.
	Ps(ctx context.Context, services ...string) ([]ComposeService, error)
	// Config validates the compose files, failing with compose's message if
	// they are invalid. Unless quiet, it also returns the merged model as
	// YAML, with ${VAR} references left as they are so that no secret value
	// appears in it.
	Config(ctx context.Context, quiet bool) ([]byte, error)
	// Passthrough runs `docker compose ARGS...` with the stack's files and
	// environment, for `pic-sure compose -- ARGS`. Nothing is added after
	// the global flags, not even --progress.
	Passthrough(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// Progress is the format compose reports progress in (`--progress`).
type Progress string

const (
	// ProgressPlain is line-oriented text, for the plain and TUI output
	// modes.
	ProgressPlain Progress = "plain"
	// ProgressJSON is one JSON object per line, for --json. It needs
	// compose 2.29.0 or later.
	ProgressJSON Progress = "json"
)

// ComposeUpOpts configures Composer.Up.
type ComposeUpOpts struct {
	// Services to start, along with their dependencies. Empty means all.
	Services []string
	// Wait adds --wait: return only once the services are running and
	// healthy, and fail if one exits or turns unhealthy instead.
	Wait bool
	// WaitTimeout bounds Wait, rounded up to whole seconds. Zero leaves
	// only ctx to bound it. It is ignored without Wait.
	WaitTimeout time.Duration
	// Out receives compose's output, mostly progress; nil discards it.
	Out io.Writer
}

// ComposeDownOpts configures Composer.Down.
type ComposeDownOpts struct {
	// Volumes adds --volumes: also remove the stack's named volumes.
	// Compose never removes external volumes, such as a shared HPDS data
	// set.
	Volumes bool
	// Out receives compose's output; nil discards it.
	Out io.Writer
}

// ComposeRunOpts configures Composer.Run.
type ComposeRunOpts struct {
	Service string
	// Args replace the service's command. Empty means its default command.
	Args []string
	// Rm adds --rm: remove the container when it exits.
	Rm bool
	// Stdout and Stderr receive the container's output, and compose's own
	// on stderr; nil discards it.
	Stdout, Stderr io.Writer
}

// ComposeExecOpts configures Composer.Exec.
type ComposeExecOpts struct {
	Service string
	// Args are the command to run and its arguments.
	Args []string
	// Stdin is the command's standard input; nil means empty input.
	Stdin io.Reader
	// Stdout and Stderr receive the command's output; nil discards it.
	Stdout, Stderr io.Writer
}

// ComposeLogsOpts configures Composer.Logs.
type ComposeLogsOpts struct {
	// Services whose logs to show. Empty means all.
	Services []string
	// Follow keeps streaming new lines until ctx ends.
	Follow bool
	// Tail limits each service's log to its last Tail lines. Zero or less
	// means the whole log.
	Tail int
	// Out receives the logs; nil discards them.
	Out io.Writer
}

// PsTimeout bounds Composer.Ps (spec §10.2), so a wedged daemon can't hang
// a status poll.
const PsTimeout = 10 * time.Second

// ErrNotRendered is returned (wrapped) by NewCompose for a stack that has no
// rendered compose file yet.
var ErrNotRendered = errors.New("the stack has not been rendered")

// The compose files' paths inside a stack directory (spec §6.1).
const (
	renderedComposeFile = ".pic-sure/render/compose.yaml"
	overridesDir        = "overrides"
)

// Compose is the Composer, built on a Runner.
type Compose struct {
	// Runner runs docker.
	Runner Runner
	// Files are passed as -f, in order: the rendered compose file, then
	// the stack's overrides. A Compose with no Files refuses to run, so
	// compose never falls back to a compose file in the working directory.
	Files []string
	// ProjectDir is the stack directory. It is compose's
	// --project-directory, which relative paths in override files resolve
	// against, and each call's working directory. Empty leaves both at
	// their defaults.
	ProjectDir string
	// Env returns the NAME=value entries each call gets in Cmd.Env: every
	// value the compose files' ${VAR} references need, secrets included.
	// It is called once per call; nil means none. There is never a .env
	// file: compose gets --env-file /dev/null, so a stray .env in the
	// project directory can't change the project name or any value.
	// Compose therefore relies on the Runner's base environment not
	// passing the user's COMPOSE_* variables through.
	Env func() []string
	// Progress is the --progress format for the verbs that report
	// progress: Up, Down, Stop, Restart, Pull and Run. Empty means
	// ProgressPlain.
	Progress Progress
}

var _ Composer = (*Compose)(nil)

// NewCompose returns the Compose for the stack in dir. Its Files are the
// rendered compose file, then every *.yaml file in dir/overrides in lexical
// order (dotfiles and directories are skipped). It fails with ErrNotRendered
// if the stack has no rendered compose file. Set Progress on the result to
// match the output mode.
func NewCompose(r Runner, dir string, env func() []string) (*Compose, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	rendered := filepath.Join(dir, filepath.FromSlash(renderedComposeFile))
	if _, err := os.Stat(rendered); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s does not exist", ErrNotRendered, rendered)
		}
		return nil, err
	}
	overrides, err := overrideFiles(filepath.Join(dir, overridesDir))
	if err != nil {
		return nil, err
	}
	return &Compose{
		Runner:     r,
		Files:      append([]string{rendered}, overrides...),
		ProjectDir: dir,
		Env:        env,
	}, nil
}

// overrideFiles lists the *.yaml files in dir in lexical order. A missing
// dir has none.
func overrideFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir) // sorted by name
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading compose overrides: %w", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files, nil
}

// Up implements Composer.
func (c *Compose) Up(ctx context.Context, opts ComposeUpOpts) error {
	args := []string{"up", "-d"}
	if opts.Wait {
		args = append(args, "--wait")
		if opts.WaitTimeout > 0 {
			secs := (opts.WaitTimeout + time.Second - 1) / time.Second
			args = append(args, "--wait-timeout", strconv.FormatInt(int64(secs), 10))
		}
	}
	return c.stream(ctx, opts.Out, true, append(args, opts.Services...))
}

// Down implements Composer.
func (c *Compose) Down(ctx context.Context, opts ComposeDownOpts) error {
	args := []string{"down", "--remove-orphans"}
	if opts.Volumes {
		args = append(args, "--volumes")
	}
	return c.stream(ctx, opts.Out, true, args)
}

// Stop implements Composer.
func (c *Compose) Stop(ctx context.Context, out io.Writer, services ...string) error {
	return c.stream(ctx, out, true, append([]string{"stop"}, services...))
}

// Restart implements Composer.
func (c *Compose) Restart(ctx context.Context, out io.Writer, services ...string) error {
	return c.stream(ctx, out, true, append([]string{"restart"}, services...))
}

// Pull implements Composer.
func (c *Compose) Pull(ctx context.Context, out io.Writer, services ...string) error {
	return c.stream(ctx, out, true, append([]string{"pull"}, services...))
}

// Logs implements Composer.
func (c *Compose) Logs(ctx context.Context, opts ComposeLogsOpts) error {
	args := []string{"logs"}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	return c.stream(ctx, opts.Out, false, append(args, opts.Services...))
}

// Run implements Composer. The container gets no TTY (-T), so its output
// can be captured whatever the writers are.
func (c *Compose) Run(ctx context.Context, opts ComposeRunOpts) (int, error) {
	if opts.Service == "" {
		return 0, errors.New("docker compose run: needs a service")
	}
	args := []string{"run"}
	if opts.Rm {
		args = append(args, "--rm")
	}
	args = append(args, "-T", opts.Service)
	return c.workload(ctx, true, append(args, opts.Args...), nil, opts.Stdout, opts.Stderr)
}

// Exec implements Composer. The command gets no TTY (-T).
func (c *Compose) Exec(ctx context.Context, opts ComposeExecOpts) (int, error) {
	if opts.Service == "" || len(opts.Args) == 0 {
		return 0, errors.New("docker compose exec: needs a service and a command")
	}
	args := append([]string{"exec", "-T", opts.Service}, opts.Args...)
	return c.workload(ctx, false, args, opts.Stdin, opts.Stdout, opts.Stderr)
}

// Ps implements Composer.
func (c *Compose) Ps(ctx context.Context, services ...string) ([]ComposeService, error) {
	cmd, err := c.cmd(false, append([]string{"ps", "--all", "--format", "json"}, services...))
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, PsTimeout)
	defer cancel()
	res, err := runCmd(pctx, c.Runner, cmd)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("docker compose ps did not finish within %s: %w", PsTimeout, err)
		}
		return nil, err
	}
	return ParseComposePs(res.Stdout)
}

// Config implements Composer.
func (c *Compose) Config(ctx context.Context, quiet bool) ([]byte, error) {
	args := []string{"config", "--no-interpolate"}
	if quiet {
		// Validation needs interpolation, to catch a required variable
		// that is unset.
		args = []string{"config", "--quiet"}
	}
	cmd, err := c.cmd(false, args)
	if err != nil {
		return nil, err
	}
	res, err := runCmd(ctx, c.Runner, cmd)
	if err != nil || quiet {
		return nil, err
	}
	return res.Stdout, nil
}

// Passthrough implements Composer.
func (c *Compose) Passthrough(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd, err := c.cmd(false, args)
	if err != nil {
		return 0, err
	}
	cmd.Stdin = stdin
	return c.Runner.Stream(ctx, cmd, stdout, stderr)
}

// cmd builds `docker compose` with the stack's global flags, then args.
// progress adds --progress, for the verbs that report it.
func (c *Compose) cmd(progress bool, args []string) (Cmd, error) {
	if len(c.Files) == 0 {
		return Cmd{}, errors.New("docker compose: no compose files")
	}
	argv := []string{"docker", "compose"}
	for _, f := range c.Files {
		argv = append(argv, "-f", f)
	}
	if c.ProjectDir != "" {
		argv = append(argv, "--project-directory", c.ProjectDir)
	}
	argv = append(argv, "--env-file", os.DevNull)
	if progress {
		p := c.Progress
		if p == "" {
			p = ProgressPlain
		}
		argv = append(argv, "--progress", string(p))
	}
	var env []string
	if c.Env != nil {
		env = slices.Clone(c.Env())
	}
	return Cmd{Argv: append(argv, args...), Env: env, Dir: c.ProjectDir}, nil
}

// stream runs a verb with its output copied to out and turns a non-zero exit
// into an *ExitError.
func (c *Compose) stream(ctx context.Context, out io.Writer, progress bool, args []string) error {
	cmd, err := c.cmd(progress, args)
	if err != nil {
		return err
	}
	code, tail, err := streamCmd(ctx, c.Runner, cmd, out, out)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError(cmd.Argv, code, tail)
	}
	return nil
}

// workload runs a verb that runs a command in a container and returns that
// command's exit code, with an error only if docker itself failed.
func (c *Compose) workload(ctx context.Context, progress bool, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd, err := c.cmd(progress, args)
	if err != nil {
		return 0, err
	}
	cmd.Stdin = stdin
	code, tail, err := streamCmd(ctx, c.Runner, cmd, stdout, stderr)
	if err != nil {
		return code, err
	}
	return workloadResult(cmd.Argv, code, tail)
}
