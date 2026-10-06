package cli

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
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
	// StartTUI runs the full-screen TUI.
	StartTUI func(tui.Options) error

	// running is set when a command's RunE starts. An error from before
	// that point came from cobra rejecting the command line, so Run reports
	// it as a usage error.
	running bool
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
// with an *exitcode.Error cause (main does this for SIGINT and SIGTERM) and
// the command failed, the cause decides the code.
func (a *App) Run(ctx context.Context, args []string) int {
	return a.execute(ctx, newRootCmd(a), args)
}

func (a *App) execute(ctx context.Context, root *cobra.Command, args []string) int {
	root.SetArgs(args)
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)

	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return exitcode.CodeOK
	}
	var coded *exitcode.Error
	if cause := context.Cause(ctx); cause != nil && errors.As(cause, &coded) {
		err = cause
	} else if !a.running && !errors.As(err, &coded) {
		// cobra rejected the command line: an unknown command or flag, a
		// bad argument count, a missing required flag.
		err = exitcode.Usage("%w", err)
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
func (a *App) startTUI() error {
	dir := a.Global.Stack
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = wd
	}
	return a.StartTUI(tui.Options{
		Root:       dir,
		Start:      tui.ScreenLanding,
		Animations: tui.AnimationsEnabled(a.Global.NoAnimations, os.Getenv),
	})
}
