package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/log"
	"github.com/JamesPeck/pic-sure-cli/internal/progress"
	"github.com/JamesPeck/pic-sure-cli/internal/tui"
)

// outputMode is how a command reports to the user (spec §10.3).
type outputMode int

const (
	// modeTUI is the TUI renderer, for a terminal.
	modeTUI outputMode = iota
	// modePlain is timestamped lines on stderr.
	modePlain
	// modeJSON is NDJSON events on stdout, or one object for a report.
	modeJSON
)

// selectMode picks the output mode (spec §10.3). terminal is whether stdin
// and stdout are both terminals.
func selectMode(g GlobalOptions, terminal bool, getenv func(string) string) outputMode {
	switch {
	case g.JSON:
		return modeJSON
	case g.Plain || !terminal || inCI(getenv):
		return modePlain
	default:
		return modeTUI
	}
}

// inCI reports whether CI is set to anything but a false value (CI=false
// and CI=0 turn it off).
func inCI(getenv func(string) string) bool {
	v := getenv("CI")
	if v == "" {
		return false
	}
	on, err := strconv.ParseBool(v)
	return err != nil || on
}

// useColor reports whether plain output may use colour. NO_COLOR turns it
// off whatever its value (https://no-color.org).
func useColor(terminal bool, getenv func(string) string) bool {
	return terminal && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

// output is one command run's output. App.output creates it on first use:
// when the command first needs its sink, or when its error is reported.
type output struct {
	mode outputMode
	sink *runSink
	// tui is the TUI renderer, when the run has one.
	tui *progress.Renderer
	// report is finish's report: the data of the success Result.
	report any
	// final is set once the run's Result is emitted or its report printed,
	// so the run never writes a second one.
	final bool
}

func (a *App) output() *output {
	if a.out == nil {
		mode := selectMode(a.Global, a.IsTerminal(), os.Getenv)
		o := &output{mode: mode}
		var sink events.Sink
		switch {
		case mode == modeJSON:
			sink = events.NewNDJSON(a.Stdout)
		case mode == modeTUI && isTerminalWriter(a.Stderr) && os.Getenv("TERM") != "dumb":
			o.tui = progress.NewRenderer(progress.RendererOptions{
				Animations: tui.AnimationsEnabled(a.Global.NoAnimations, os.Getenv),
				Interrupt:  a.interrupt,
				// A forced quit skips the command's cleanups, as main's
				// default action for a second SIGINT does.
				Force:   func() { os.Exit(exitcode.CodeInterrupted) },
				Input:   a.Stdin,
				Output:  a.Stderr,
				NoColor: os.Getenv("NO_COLOR") != "",
				LogPath: func() string { return a.runLogPath },
			})
			a.tuiOut.Store(o.tui)
			sink = o.tui
		default:
			sink = events.NewPlain(a.Stderr, events.PlainOptions{
				Color: useColor(isTerminalWriter(a.Stderr), os.Getenv),
			})
		}
		o.sink = &runSink{Sink: sink}
		a.out = o
	}
	return a.out
}

// endTUI ends the TUI renderer, if the run has one, so that what the
// command prints next goes below its last frame.
func (o *output) endTUI() {
	if o.tui != nil {
		o.tui.Close()
	}
}

// newSink returns the event sink for the run's output mode. Every call in
// one run returns the same sink, so the Result the cli layer emits follows
// the operation's events through the same renderer.
func (a *App) newSink() events.Sink { return a.output().sink }

// finish records a streaming command's report. If the command then returns
// nil, the run ends with a success Result carrying it, which --json prints
// as the last line on stdout. In the other modes, text (if not nil) writes
// the human summary to stdout now. A command ends with
// `return a.finish(report, text)`.
func (a *App) finish(report any, text func(io.Writer) error) error {
	o := a.output()
	if o.mode == modeJSON {
		// Encode now, so a report that can't be encoded fails the command
		// instead of producing a successful result without its data.
		if _, err := json.Marshal(report); err != nil {
			return exitcode.Failed("encoding the result: %w", err)
		}
	}
	o.report = report
	if o.mode == modeJSON || text == nil {
		return nil
	}
	o.endTUI()
	return text(a.Stdout)
}

// printReport prints a read-only command's report (status, doctor,
// version): with --json, report as one JSON object with schema_version 2 on
// stdout; otherwise the text that text writes to stdout. No Result follows
// the JSON report, even if the command then fails (doctor with a failing
// check).
func (a *App) printReport(report any, text func(io.Writer) error) error {
	o := a.output()
	if o.mode != modeJSON {
		o.endTUI()
		return text(a.Stdout)
	}
	if err := events.WriteReport(a.Stdout, report); err != nil {
		return exitcode.Failed("%w", err)
	}
	o.final = true
	return nil
}

// succeed ends a run whose command returned nil. A run that used the output
// layer gets its success Result, unless it printed a report; help and
// completion, which never touch it, print only their text. Output the
// renderer couldn't write fails the run.
func (a *App) succeed() error {
	o := a.out
	if o == nil {
		return nil
	}
	if err := o.sink.writeErr(); err != nil {
		return exitcode.Failed("writing output: %w", err)
	}
	if !o.final {
		o.final = true
		o.sink.Emit(events.Result{OK: true, Data: o.report})
		if err := o.sink.writeErr(); err != nil {
			return exitcode.Failed("writing output: %w", err)
		}
	}
	return nil
}

// reportError tells the user why cmd failed: "pic-sure: <err>" on stderr.
// Unless the run already ended with a Result or a report, it also emits a
// failed Result, which --json prints as the last line on stdout: the exit
// code, the message, and the first step that failed. A usage error gets the
// "Run --help" hint when the command line itself was wrong: cobra rejected
// it, or the command marked its error with withUsageHint. The message is
// redacted as logs are (log.Redact), since an error can quote a secret.
func (a *App) reportError(cmd *cobra.Command, err error) {
	code := exitcode.FromError(err)
	msg := log.Redact(err.Error())
	if o := a.output(); !o.final {
		o.final = true
		o.sink.Emit(events.Result{Error: &events.ErrorInfo{
			ExitCode: code,
			Message:  msg,
			Step:     o.sink.failedStep(),
		}})
	}
	_, _ = fmt.Fprintf(a.Stderr, "pic-sure: %s\n", msg)
	var hint usageHint
	if code == exitcode.CodeUsage && cmd != nil && (!a.running || errors.As(err, &hint)) {
		_, _ = fmt.Fprintf(a.Stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
	}
}

// withUsageHint marks a usage error that a command raised about its own
// command line (a missing subcommand, an unknown key argument), so it gets
// the "Run --help" hint. Other usage errors, such as an invalid config
// file, don't.
func withUsageHint(err error) error { return usageHint{err} }

type usageHint struct{ error }

func (h usageHint) Unwrap() error { return h.error }

// jsonRequested reports whether args ask for --json, for a command line
// cobra rejected, perhaps before it reached that flag. It stops at "--", and
// the last --json or --json=BOOL wins. It is a best guess: it can't tell a
// --json that another flag took as its value, or that a command took as a
// positional argument, from the flag itself.
func jsonRequested(args []string) bool {
	on := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			on = true
		} else if v, ok := strings.CutPrefix(arg, "--json="); ok {
			on, _ = strconv.ParseBool(v)
		}
	}
	return on
}

// runSink is the run's sink: the renderer, plus the first step that failed
// (for the failed Result) and the renderer's write error.
type runSink struct {
	events.Sink

	mu     sync.Mutex
	failed string
}

func (s *runSink) Emit(e events.Event) {
	if d, ok := e.(events.StepDone); ok && d.Status == events.StepFailed {
		s.mu.Lock()
		if s.failed == "" {
			s.failed = d.ID
		}
		s.mu.Unlock()
	}
	s.Sink.Emit(e)
}

func (s *runSink) failedStep() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// writeErr is the renderer's first write error, if it keeps one.
func (s *runSink) writeErr() error {
	if r, ok := s.Sink.(interface{ Err() error }); ok {
		return r.Err()
	}
	return nil
}
