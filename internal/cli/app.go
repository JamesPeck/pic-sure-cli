package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/progress"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
	"github.com/JamesPeck/pic-sure-cli/internal/tty"
	"github.com/JamesPeck/pic-sure-cli/internal/tui"
)

// BuildInfo is the version metadata injected with -ldflags.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// App is what every command shares: build info, the parsed global flags,
// the standard streams, and seams for tests.
type App struct {
	Info   BuildInfo
	Global GlobalOptions

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// IsTerminal reports whether stdin and stdout are both terminals.
	IsTerminal func() bool
	// StartTUI runs the full-screen TUI until the user quits or ctx is done.
	StartTUI func(context.Context, tui.Options) error

	// migrations replaces stack.ConfigMigrations() in tests (gate.go).
	migrations *stack.Registry

	// running is set when a command's RunE starts. An error from before
	// that point came from cobra rejecting the command line, so Run reports
	// it as a usage error.
	running bool
	// out is the run's output mode and event sink (output.go), created on
	// first use.
	out *output
	// runLog is the running command's logging (logging.go), from the start
	// of its RunE until Run returns, and runLogPath its file, once open.
	runLog     *log.Run
	runLogPath string
	// tuiOut is the run's TUI renderer, if it has one, for log records
	// written from any goroutine (logStderr).
	tuiOut atomic.Pointer[progress.Renderer]
	// interrupt cancels the running command's context as SIGINT would. The
	// TUI renderer calls it when the user confirms Ctrl-C, which the
	// terminal delivers as a key rather than a signal while it runs.
	interrupt func()
}

// NewApp returns an App wired to the process's streams and terminal.
func NewApp(info BuildInfo) *App {
	return &App{
		Info:       info,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		IsTerminal: tty.IsInteractive,
		StartTUI:   tui.Run,
	}
}

// Execute runs the CLI with args (without the program name) and returns the
// process exit code.
func Execute(ctx context.Context, info BuildInfo, args []string) int {
	return NewApp(info).Run(ctx, args)
}

// Run runs one command line and returns its exit code. If ctx was cancelled
// with an *exitcode.Error cause (main does this for SIGINT and SIGTERM), that
// cause decides the code even when the command returned cleanly: an
// interrupted command exits 128+N (spec §10.4). The message is the cause's,
// unless the command's error wraps the cause and so says more, such as the
// step to resume from (steps.Error).
func (a *App) Run(ctx context.Context, args []string) int {
	return a.execute(ctx, newRootCmd(a), args)
}

func (a *App) execute(ctx context.Context, root *cobra.Command, args []string) int {
	a.running = false
	a.out = nil
	a.tuiOut.Store(nil)
	a.runLogPath = ""
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	a.interrupt = func() { cancel(exitcode.Signaled(os.Interrupt)) }
	root.SetArgs(args)
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)

	cmd, err := root.ExecuteContextC(ctx)
	defer func() { a.endRunLog(err) }()
	var coded *exitcode.Error
	switch cause := context.Cause(ctx); {
	case cause != nil && errors.As(cause, &coded):
		if errors.Is(err, cause) {
			err = &exitcode.Error{Code: coded.Code, Err: err}
		} else {
			err = cause
		}
	case err == nil:
		err = a.succeed()
	case !a.running && !errors.As(err, &coded):
		// cobra may have stopped before it reached --json.
		a.Global.JSON = jsonRequested(args)
		err = exitcode.Usage("%w", err)
	}
	if err == nil {
		return exitcode.CodeOK
	}
	a.reportError(cmd, err)
	return exitcode.FromError(err)
}

// canPrompt reports whether the command may ask the user anything: stdin
// and stdout are terminals, and no flag rules prompts out.
func (a *App) canPrompt() bool {
	g := a.Global
	return !g.Yes && !g.NonInteractive && !g.JSON && a.IsTerminal()
}

// startTUI opens the TUI's landing screen on the --stack directory, or the
// current one.
func (a *App) startTUI(ctx context.Context) error {
	dir := a.Global.Stack
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = wd
	}
	return a.StartTUI(ctx, tui.Options{
		Root:       dir,
		Start:      tui.ScreenLanding,
		Animations: tui.AnimationsEnabled(a.Global.NoAnimations, os.Getenv),
	})
}
