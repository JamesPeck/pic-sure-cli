package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/dashboard"
	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/tui"
)

// dashLogTail is how many past lines the dashboard's log pane starts with.
const dashLogTail = 200

// child returns an App for one command the TUI runs in-process. It shares
// a's build info and config migrations, has no terminal, and writes to
// stdout and stderr.
func (a *App) child(stdout, stderr io.Writer) *App {
	return &App{
		Info:       a.Info,
		Stdin:      strings.NewReader(""),
		Stdout:     stdout,
		Stderr:     stderr,
		IsTerminal: func() bool { return false },
		migrations: a.migrations,
	}
}

// dashBackend is the dashboard's dashboard.Backend: ps, status and logs on
// the stack in dir, as those commands would run them, without a run log.
type dashBackend struct {
	a   *App
	dir string
}

var _ dashboard.Backend = dashBackend{}

// open opens the stack as the command name would: its version gate, and a
// compose client with the stack's environment. Warnings are dropped.
func (b dashBackend) open(ctx context.Context, name string) (*App, *cobra.Command, *stack.Stack, error) {
	c := b.a.child(io.Discard, io.Discard)
	cmd, _, err := newRootCmd(c).Find([]string{name})
	if err != nil {
		return nil, nil, nil, err
	}
	// Registering the global flags reset c.Global to the defaults.
	c.Global.Stack = b.dir
	cmd.SetContext(ctx)
	st, err := c.openStackUnlogged(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	return c, cmd, st, nil
}

func (b dashBackend) compose(ctx context.Context, name string) (*docker.Compose, func(), error) {
	c, cmd, st, err := b.open(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	comp, err := c.stackCompose(cmd, c.newDeps().Runner, st)
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return comp, func() { _ = st.Close() }, nil
}

func (b dashBackend) Services(ctx context.Context) ([]ops.StatusService, error) {
	comp, done, err := b.compose(ctx, "ps")
	if err != nil {
		return nil, redacted(err)
	}
	defer done()
	ps, err := comp.Ps(ctx)
	if err != nil {
		return nil, redacted(err)
	}
	return ops.StatusServices(ps), nil
}

func (b dashBackend) Status(ctx context.Context, deep bool) (*ops.StatusReport, error) {
	c, cmd, st, err := b.open(ctx, "status")
	if err != nil {
		return nil, redacted(err)
	}
	defer func() { _ = st.Close() }()
	return c.statusReport(cmd, st, deep), nil
}

func (b dashBackend) FollowLogs(ctx context.Context, service string, w io.Writer) error {
	comp, done, err := b.compose(ctx, "logs")
	if err != nil {
		return redacted(err)
	}
	defer done()
	// Compose's own messages stay out of the pane; the end of them is in
	// the error.
	err = comp.Logs(ctx, docker.ComposeLogsOpts{Services: []string{service}, Follow: true, Tail: dashLogTail, Out: w, Err: io.Discard})
	return redacted(err)
}

// redacted is err with its message redacted, as reportError prints it:
// compose had the secrets in its environment, and its error can quote one.
func redacted(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(log.Redact(err.Error()))
}

// commandFromTUI is tui.Options.Command: `pic-sure --stack DIR ARGS...`
// (with no stack at all when req.Dir is empty) run in-process for a landing or
// dashboard action or a load. Its events, and the run log's
// stderr records, go to req.Sink; the summary it would print is the
// result's Summary. Its exit code and message come back as the error.
func (a *App) commandFromTUI(ctx context.Context, req tui.CommandRequest) (tui.InitResult, error) {
	var out, errOut bytes.Buffer
	var result *events.Result
	c := a.actionApp(req, &out, &errOut, func(r events.Result) { result = &r })
	var args []string
	if req.Dir == "" {
		c.noStack = true
	} else {
		args = append(args, "--stack", req.Dir)
	}
	if a.Global.WaitLock {
		args = append(args, "--wait-lock")
	}
	if a.Global.LogLevel != "" {
		args = append(args, "--log-level", a.Global.LogLevel)
	}
	args = append(args, req.Args...)
	code := c.execute(ctx, newRootCmd(c), args)
	res := tui.InitResult{Summary: out.String(), LogPath: c.runLogPath}
	if code == exitcode.CodeOK {
		return res, nil
	}
	msg := strings.TrimPrefix(strings.TrimSpace(errOut.String()), "pic-sure: ")
	if result != nil && result.Error != nil {
		msg = result.Error.Message
	}
	return res, &exitcode.Error{Code: code, Err: errors.New(msg)}
}

// actionApp is the child App commandFromTUI runs req on: its summary goes
// to stdout, its warnings, log records and other events to req.Sink, and
// its error to rest and to onResult; the gate asks through req.Confirm.
func (a *App) actionApp(req tui.CommandRequest, stdout, rest io.Writer, onResult func(events.Result)) *App {
	c := a.child(stdout, &warnEvents{sink: redactingSink{req.Sink}, rest: rest})
	c.tuiSink = events.SinkFunc(func(e events.Event) {
		if r, ok := e.(events.Result); ok {
			onResult(r)
			return
		}
		req.Sink.Emit(e)
	})
	c.tuiLog.Store(&logEvents{redactingSink{req.Sink}})
	c.tuiConfirm = req.Confirm
	return c
}

// warnEvents is an in-process command's stderr: its warnings become Warning
// events, and the rest (the error, which the Result also carries) goes to
// rest.
type warnEvents struct {
	sink events.Sink
	rest io.Writer
}

func (w *warnEvents) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if text, ok := strings.CutPrefix(line, "pic-sure: warning: "); ok {
			w.sink.Emit(events.Warning{Text: text})
		} else {
			_, _ = io.WriteString(w.rest, line+"\n")
		}
	}
	return len(b), nil
}
