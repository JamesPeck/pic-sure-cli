package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// outputMode is how a command reports to the user (spec §10.3).
type outputMode int

const (
	// modeTUI is the TUI renderer, for a terminal. Until ticket 038 adds
	// it, TUI mode renders like plain.
	modeTUI outputMode = iota
	// modePlain is timestamped lines on stderr.
	modePlain
	// modeJSON is NDJSON events on stdout, or one object for a report.
	modeJSON
)

// selectMode picks the output mode: JSON for --json, plain for --plain, no
// terminal (stdin and stdout both, as for prompting) or a CI environment,
// and the TUI otherwise.
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

// useColor reports whether plain output to a writer that is (or isn't) a
// terminal may use colour: not when NO_COLOR is set to anything
// (https://no-color.org) or TERM is dumb.
func useColor(terminal bool, getenv func(string) string) bool {
	return terminal && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

// isTerminalWriter reports whether w is a terminal.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

// output is one command run's output: its mode, its event sink, and
// whether the command has already emitted its Result or printed its report.
// App.output creates it on first use: when the command first needs its
// sink, or when its error is reported.
type output struct {
	mode  outputMode
	sink  *stepTracker
	final bool
}

// output returns the run's output, creating it on first use.
func (a *App) output() *output {
	if a.out == nil {
		mode := selectMode(a.Global, a.IsTerminal(), os.Getenv)
		var sink events.Sink
		switch mode {
		case modeJSON:
			sink = events.NewNDJSON(a.Stdout)
		default: // modeTUI falls back to plain until ticket 038
			sink = events.NewPlain(a.Stderr, events.PlainOptions{
				Color: useColor(isTerminalWriter(a.Stderr), os.Getenv),
			})
		}
		a.out = &output{mode: mode, sink: &stepTracker{Sink: sink}}
	}
	return a.out
}

// newSink returns the event sink for the run's output mode. Every call in
// one run returns the same sink, so the Result the cli layer emits follows
// the operation's events through the same renderer.
func (a *App) newSink() events.Sink { return a.output().sink }

// finish reports a command's success by emitting the final Result with
// data, the command's report (nil if it has none); --json prints it as the
// last line on stdout. A command that streams events ends with
// `return a.finish(report)`.
func (a *App) finish(data any) error {
	o := a.output()
	if o.mode == modeJSON && data != nil {
		// Encode first, so a report that can't be encoded fails the command
		// instead of producing a successful result without its data.
		if _, err := json.Marshal(data); err != nil {
			return exitcode.Failed("encoding the result: %w", err)
		}
	}
	o.sink.Emit(events.Result{OK: true, Data: data})
	o.final = true
	return nil
}

// printReport prints a read-only command's report (status, doctor,
// version): with --json, report as one JSON object with schema_version 2 on
// stdout; otherwise the text that text writes to stdout. A command that
// prints a report and then fails (doctor with a failing check) gets no
// result line after it.
func (a *App) printReport(report any, text func(io.Writer) error) error {
	o := a.output()
	if o.mode != modeJSON {
		return text(a.Stdout)
	}
	if err := events.WriteReport(a.Stdout, report); err != nil {
		return exitcode.Failed("%w", err)
	}
	o.final = true
	return nil
}

// reportError tells the user why cmd failed: "pic-sure: <err>" on stderr,
// with a usage hint for a usage error. Unless the command already wrote its
// result or report, it also emits a failed Result, which --json prints as
// the last line on stdout: the exit code, the message, and the first step
// that failed.
func (a *App) reportError(cmd *cobra.Command, err error) {
	code := exitcode.FromError(err)
	if o := a.output(); !o.final {
		o.final = true
		o.sink.Emit(events.Result{Error: &events.ErrorInfo{
			ExitCode: code,
			Message:  err.Error(),
			Step:     o.sink.failedStep(),
		}})
	}
	_, _ = fmt.Fprintf(a.Stderr, "pic-sure: %v\n", err)
	if code == exitcode.CodeUsage && cmd != nil {
		_, _ = fmt.Fprintf(a.Stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
	}
}

// stepTracker passes events through to Sink and remembers the first step
// that failed, for the failed Result.
type stepTracker struct {
	events.Sink

	mu     sync.Mutex
	failed string
}

func (t *stepTracker) Emit(e events.Event) {
	if d, ok := e.(events.StepDone); ok && d.Status == events.StepFailed {
		t.mu.Lock()
		if t.failed == "" {
			t.failed = d.ID
		}
		t.mu.Unlock()
	}
	t.Sink.Emit(e)
}

func (t *stepTracker) failedStep() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failed
}
