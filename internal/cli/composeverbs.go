package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/render"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func newDownCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the stack's containers (volumes are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.composeVerb(cmd, "down", "Stop the stack", func(d *ops.Deps, st *stack.Stack, out io.Writer) error {
				// A helper a killed command left would keep the data
				// network, which compose down removes.
				cfg, err := st.LoadConfig()
				if err != nil {
					return configError(err)
				}
				helperErr := ops.RemoveHelperContainers(cmd.Context(), d, d.Sink, "down", st, cfg.Name, nil)
				return errors.Join(helperErr, d.Compose.Down(cmd.Context(), docker.ComposeDownOpts{Out: out}))
			})
		},
	}
}

func newRestartCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "restart [SERVICE...]",
		Short: "Restart services (default: all)",
		RunE: func(cmd *cobra.Command, args []string) error {
			title := "Restart every service"
			if len(args) > 0 {
				title = "Restart " + strings.Join(args, ", ")
			}
			return a.composeVerb(cmd, "restart", title, func(d *ops.Deps, _ *stack.Stack, out io.Writer) error {
				return d.Compose.Restart(cmd.Context(), out, args...)
			})
		},
	}
}

// composeVerb runs a mutating compose verb under the stack lock, as step id
// with title, compose's output (mostly progress, on stderr) becoming the
// step's Log events.
func (a *App) composeVerb(cmd *cobra.Command, id, title string, verb func(*ops.Deps, *stack.Stack, io.Writer) error) error {
	st, err := a.openStack(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	d := a.newDeps()
	lock, err := a.lockStack(cmd.Context(), cmd, st, d.Sink)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	if d.Compose, err = a.stackCompose(cmd, d.Runner, st); err != nil {
		return err
	}
	if err := checkOwnedStack(cmd, d, st); err != nil {
		return err
	}

	err = sinkStep(d.Sink, id, title, func(_, errOut io.Writer) error { return verb(d, st, errOut) })
	if err != nil {
		return err
	}
	return a.finish(nil, nil)
}

// sinkStep runs fn as step id with title on sink. fn's writers turn
// subprocess output into the step's Log events.
func sinkStep(sink events.Sink, id, title string, fn func(out, errOut io.Writer) error) error {
	sink.Emit(events.StepStarted{ID: id, Title: title})
	out := events.NewLogWriter(sink, id, events.StreamStdout)
	errOut := events.NewLogWriter(sink, id, events.StreamStderr)
	err := fn(out, errOut)
	_ = out.Close()
	_ = errOut.Close()
	status := events.StepOK
	if err != nil {
		status = events.StepFailed
	}
	sink.Emit(events.StepDone{ID: id, Status: status})
	return err
}

// psReport is ps's --json report, in status's shape.
type psReport struct {
	Services []ops.StatusService `json:"services"`
}

func newPsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "List the stack's containers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.openStack(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			d := a.newDeps()
			c, err := a.stackCompose(cmd, d.Runner, st)
			if err != nil {
				return err
			}
			a.warnForeign(cmd.Context(), d, st)
			ps, err := c.Ps(cmd.Context())
			if err != nil {
				return err
			}
			report := psReport{Services: ops.StatusServices(ps)}
			return a.printReport(report, func(w io.Writer) error { return writePs(w, report.Services) })
		},
	}
}

func writePs(w io.Writer, services []ops.StatusService) error {
	if len(services) == 0 {
		_, err := io.WriteString(w, "No containers.\n")
		return err
	}
	var b strings.Builder
	row := func(svc, state, health, status string) {
		fmt.Fprintf(&b, "%-28s %-10s %-10s %s\n", svc, state, health, status)
	}
	row("SERVICE", "STATE", "HEALTH", "STATUS")
	for _, s := range services {
		health := s.Health
		if health == "" {
			health = "-"
		}
		row(s.Service, s.State, health, s.Status)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func newLogsCmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "logs [SERVICE]",
		Short: "Show service logs",
		Long: `Show service logs (default: every service) on stdout. With -f, keep
following new lines until Ctrl-C. With --json, each line is a log event of
step "logs".`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			follow, _ := cmd.Flags().GetBool("follow")
			st, err := a.openStack(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			d := a.newDeps()
			c, err := a.stackCompose(cmd, d.Runner, st)
			if err != nil {
				return err
			}
			a.warnForeign(cmd.Context(), d, st)
			opts := docker.ComposeLogsOpts{Services: args, Follow: follow, Out: a.stdout(), Err: a.stderr()}
			if a.output().mode != modeJSON {
				err = c.Logs(cmd.Context(), opts)
			} else {
				err = sinkStep(d.Sink, "logs", "Show service logs", func(out, errOut io.Writer) error {
					opts.Out, opts.Err = out, errOut
					return c.Logs(cmd.Context(), opts)
				})
			}
			if err != nil {
				if cause := context.Cause(cmd.Context()); cause != nil {
					// Ctrl-C is how -f ends: report the signal alone.
					return cause
				}
				return err
			}
			return a.finish(nil, nil)
		},
	}
	c.Flags().BoolP("follow", "f", false, "follow log output")
	return c
}

func newComposeCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "compose -- ARGS...",
		Short: "Run docker compose against the rendered stack (escape hatch)",
		Long: `Run docker compose against the rendered stack, with the same -f files and
environment the CLI uses. Put -- before the compose arguments. pic-sure
exits with compose's exit code. Its output is compose's own, so --json is
refused.

These subcommands run without the stack lock, and on a stack a newer
pic-sure rendered: ps, logs, top, config, events, images, ls, port,
version, exec, stats, wait (without --down-project) and attach. Any other
holds the stack lock until compose exits, as every command that can change
the stack does.`,
		Example: `  pic-sure compose -- ps -a
  pic-sure compose -- exec hpds sh`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 || len(args) == 0 {
				return withUsageHint(exitcode.Usage("put -- and then the docker compose arguments after compose"))
			}
			if a.Global.JSON {
				return exitcode.Usage("compose prints compose's own output, so it takes no --json; pass compose's --format json instead")
			}
			if flag := composeProjectFlag(args); flag != "" {
				return exitcode.Usage("compose -- %s: pic-sure runs compose on the stack's own project; use --stack DIR for another stack", flag)
			}
			st, err := a.openStack(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			d := a.newDeps()
			if commandClass(cmd) != stack.ReadOnly {
				lock, err := a.lockStack(cmd.Context(), cmd, st, d.Sink)
				if err != nil {
					return err
				}
				defer func() { _ = lock.Unlock() }()
			}
			c, err := a.stackCompose(cmd, a.newForegroundRunner(d.Log, args), st)
			if err != nil {
				return err
			}
			// -p pins the project the ownership check covers: it beats a
			// name: in a file added with -f and COMPOSE_PROJECT_NAME in an
			// --env-file.
			cfg, err := st.LoadConfig()
			switch {
			case err == nil:
				args = append([]string{"-p", cfg.Name}, args...)
			case commandClass(cmd) != stack.ReadOnly:
				return configError(err)
			}
			if commandClass(cmd) != stack.ReadOnly {
				if err := checkOwned(cmd, d, st, cfg); err != nil {
					return err
				}
			}
			// Waiting for the lock may have started the TUI; compose needs
			// the terminal.
			a.output().endTUI()
			code, err := c.Passthrough(cmd.Context(), args, a.Stdin, a.Stdout, a.Stderr)
			if cause := context.Cause(cmd.Context()); cause != nil {
				// The signal decides the exit code; compose got it too.
				return cause
			}
			if err != nil {
				return err
			}
			if code != 0 {
				return &exitcode.Error{Code: code, Err: fmt.Errorf("docker compose exited %d", code)}
			}
			return nil
		},
	}
}

// stackCompose returns the compose adapter for st, with the environment its
// compose files need (render.ComposeEnv) and the progress format for the
// output mode. A stack that hasn't been rendered is exit 3, wrapping
// docker.ErrNotRendered. A read-only command (gate.go) that can't read the
// config or secrets warns and carries on without them.
func (a *App) stackCompose(cmd *cobra.Command, r docker.Runner, st *stack.Stack) (*docker.Compose, error) {
	c, _, _, err := a.stackComposeConfig(cmd, r, st)
	return c, err
}

// stackComposeConfig is stackCompose, also returning the config and secrets
// the environment came from.
func (a *App) stackComposeConfig(cmd *cobra.Command, r docker.Runner, st *stack.Stack) (*docker.Compose, *stack.Config, *stack.Secrets, error) {
	// Secrets first: a stack without them needs init before up.
	cfg, sec, err := a.stackComposeInputs(cmd, st)
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := docker.NewCompose(r, st.Dir, nil)
	if errors.Is(err, docker.ErrNotRendered) {
		return nil, nil, nil, exitcode.Precondition("%w yet; run `pic-sure up`", docker.ErrNotRendered)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	env, err := render.ComposeEnv(cfg, sec)
	if err != nil {
		return nil, nil, nil, err
	}
	c.Env = func() []string { return env }
	if a.output().mode == modeJSON {
		c.Progress = docker.ProgressJSON
	}
	return c, cfg, sec, nil
}

func (a *App) stackComposeInputs(cmd *cobra.Command, st *stack.Stack) (*stack.Config, *stack.Secrets, error) {
	// A read-only command creates no container, so it carries on with the
	// variables set but empty, which keeps compose from warning that they
	// are unset. It must work on a stack whose config is invalid or newer
	// (§10.6), or that has no secrets.yaml yet.
	readOnly := commandClass(cmd) == stack.ReadOnly
	cfg, err := st.LoadConfig()
	if err != nil {
		if !readOnly {
			return nil, nil, configError(err)
		}
		a.warnStderr("docker compose runs without the stack's config or secrets: %v", err)
		def := stack.DefaultConfig()
		return &def, &stack.Secrets{}, nil
	}
	sec, err := st.LoadSecrets()
	if err != nil {
		if !readOnly {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil, exitcode.Precondition("the stack has no secrets.yaml; run `pic-sure init` to finish creating it")
			}
			return nil, nil, err
		}
		a.warnStderr("docker compose runs without the stack's secrets: %v", err)
		sec = &stack.Secrets{}
	}
	return cfg, sec, nil
}
