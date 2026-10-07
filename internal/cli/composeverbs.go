package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
			return a.composeVerb(cmd, "down", "Stop the stack", func(d *ops.Deps, out io.Writer) error {
				return d.Compose.Down(cmd.Context(), docker.ComposeDownOpts{Out: out})
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
			return a.composeVerb(cmd, "restart", title, func(d *ops.Deps, out io.Writer) error {
				return d.Compose.Restart(cmd.Context(), out, args...)
			})
		},
	}
}

// composeVerb runs a mutating compose verb under the stack lock, as step id
// with title, compose's output becoming the step's Log events.
func (a *App) composeVerb(cmd *cobra.Command, id, title string, verb func(*ops.Deps, io.Writer) error) error {
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
	if d.Compose, err = a.stackCompose(d.Runner, st); err != nil {
		return err
	}

	d.Sink.Emit(events.StepStarted{ID: id, Title: title})
	out := events.NewLogWriter(d.Sink, id, events.StreamStderr)
	err = verb(d, out)
	_ = out.Close()
	status := events.StepOK
	if err != nil {
		status = events.StepFailed
	}
	d.Sink.Emit(events.StepDone{ID: id, Status: status})
	if err != nil {
		return err
	}
	return a.finish(nil, nil)
}

// psReport is ps's --json report.
type psReport struct {
	Services []docker.ComposeService `json:"services"`
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
			c, err := a.stackCompose(d.Runner, st)
			if err != nil {
				return err
			}
			services, err := c.Ps(cmd.Context())
			if err != nil {
				return err
			}
			report := psReport{Services: services}
			if report.Services == nil {
				report.Services = []docker.ComposeService{}
			}
			return a.printReport(report, func(w io.Writer) error { return writePs(w, services) })
		},
	}
}

// writePs prints the services as a table.
func writePs(w io.Writer, services []docker.ComposeService) error {
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
		Long: `Show service logs (default: every service). With -f, keep following new
lines until Ctrl-C. With --json, each line is a log event.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			follow, _ := cmd.Flags().GetBool("follow")
			st, err := a.openStack(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			d := a.newDeps()
			c, err := a.stackCompose(d.Runner, st)
			if err != nil {
				return err
			}
			opts := docker.ComposeLogsOpts{Services: args, Follow: follow, Out: a.Stdout}
			if a.output().mode == modeJSON {
				lw := events.NewLogWriter(d.Sink, "logs", events.StreamStdout)
				defer func() { _ = lw.Close() }()
				opts.Out = lw
			}
			if err := c.Logs(cmd.Context(), opts); err != nil {
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
exits with compose's exit code. It doesn't take the stack lock, so it can
run alongside other commands; don't change the stack with it while one is
running.`,
		Example: `  pic-sure compose -- ps -a
  pic-sure compose -- exec hpds sh`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 || len(args) == 0 {
				return withUsageHint(exitcode.Usage("put -- and then the docker compose arguments after compose"))
			}
			st, err := a.openStack(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			d := a.newDeps()
			// In the foreground, an interactive command can read the
			// terminal.
			r := &docker.ExecRunner{Log: d.Log, Foreground: true}
			c, err := a.stackCompose(r, st)
			if err != nil {
				return err
			}
			code, err := c.Passthrough(cmd.Context(), args, a.Stdin, a.Stdout, a.Stderr)
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
// output mode. A stack that hasn't been rendered is exit 3.
func (a *App) stackCompose(r docker.Runner, st *stack.Stack) (*docker.Compose, error) {
	c, err := docker.NewCompose(r, st.Dir, nil)
	if errors.Is(err, docker.ErrNotRendered) {
		return nil, exitcode.Precondition("the stack hasn't been rendered yet; run `pic-sure up`")
	}
	if err != nil {
		return nil, err
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		return nil, configError(err)
	}
	sec, err := st.LoadSecrets()
	if err != nil {
		return nil, err
	}
	env, err := render.ComposeEnv(cfg, sec)
	if err != nil {
		return nil, err
	}
	c.Env = func() []string { return env }
	if a.output().mode == modeJSON {
		c.Progress = docker.ProgressJSON
	}
	return c, nil
}
