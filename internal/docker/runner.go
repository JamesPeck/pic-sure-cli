package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Cmd is one subprocess invocation.
type Cmd struct {
	// Argv is the program and its arguments; Argv[0] is looked up on PATH.
	// Never put a secret in Argv: other local users can see it in ps, and
	// runners log it at debug level. Pass secrets in Env or Stdin instead.
	Argv []string
	// Env holds NAME=value entries added to the runner's base environment.
	// Runners never log the values. A secret reaches docker as a bare
	// `-e NAME` in Argv plus NAME=value here.
	Env []string
	// Stdin is the process's standard input; nil means empty input.
	Stdin io.Reader
	// Dir is the working directory; empty means the CLI's own.
	Dir string
}

// Result is a finished Run.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner runs subprocesses.
//
// Both methods return a nil error whenever the process ran to completion,
// whatever its exit code: a non-zero exit is not an error at this level, and
// a process killed by signal N reports exit code 128+N. The error is non-nil
// only when the process could not be started, or when ctx ended before the
// process did, in which case it wraps ctx.Err(). Use RunChecked to treat a
// non-zero exit as an error.
type Runner interface {
	// Run waits for the process and returns its captured output.
	Run(ctx context.Context, c Cmd) (Result, error)
	// Stream copies the process's stdout and stderr to the writers as it
	// runs, without buffering all of it, and returns the exit code. A nil
	// writer discards that stream.
	Stream(ctx context.Context, c Cmd, stdout, stderr io.Writer) (int, error)
}

// ExitError is a process that ran but exited non-zero.
type ExitError struct {
	Argv     []string
	ExitCode int
	Stderr   []byte
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s exited %d", FormatArgv(e.Argv), e.ExitCode)
	if line := lastLine(e.Stderr); line != "" {
		msg += ": " + line
	}
	return msg
}

// RunChecked is r.Run that also returns an *ExitError for a non-zero exit,
// along with the Result.
func RunChecked(ctx context.Context, r Runner, c Cmd) (Result, error) {
	res, err := r.Run(ctx, c)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, &ExitError{Argv: c.Argv, ExitCode: res.ExitCode, Stderr: res.Stderr}
	}
	return res, nil
}

// maxErrLine bounds how much stderr an ExitError message carries.
const maxErrLine = 300

func lastLine(b []byte) string {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	line := strings.TrimSpace(string(lines[len(lines)-1]))
	if len(line) > maxErrLine {
		line = strings.ToValidUTF8(line[:maxErrLine], "") + "…"
	}
	return line
}

// FormatArgv renders argv for messages and logs: space-separated, with Go
// quoting for arguments that are empty or contain spaces, quotes or
// backslashes.
func FormatArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\") {
			a = strconv.Quote(a)
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}
