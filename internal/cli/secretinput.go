package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/x/term"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// stdinIsTerminal reports whether stdin is a terminal.
func (a *App) stdinIsTerminal() bool {
	if a.stdinTerminal != nil {
		return a.stdinTerminal()
	}
	f, ok := a.Stdin.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// readUserSecret reads a secret the operator supplies on stdin
// (stack.ReadUserSecret), what naming it in the prompt and source in errors.
// Piped stdin is read to EOF. A terminal would wait silently for Ctrl-D with
// the secret echoed on screen, so there it prompts on stderr and reads one
// line with echo off; --non-interactive and --json forbid the prompt, so
// they are a usage error.
func (a *App) readUserSecret(ctx context.Context, what, source string) (stack.Secret, error) {
	if !a.stdinIsTerminal() {
		return stack.ReadUserSecret(a.Stdin, source)
	}
	if a.Global.NonInteractive || a.Global.JSON {
		return "", exitcode.Usage("%s: stdin is a terminal, and %s forbids prompting; pipe the secret in instead, e.g. `< FILE`", source, noPromptFlag(a.Global))
	}
	_, _ = fmt.Fprintf(a.stderr(), "Paste the %s and press Enter (input is hidden): ", what)
	read := a.readHidden
	if read == nil {
		read = readHiddenLine
	}
	line, err := read(ctx, a.Stdin)
	_, _ = fmt.Fprintln(a.stderr())
	if err != nil {
		return "", fmt.Errorf("%s: reading the secret: %w", source, err)
	}
	return stack.ReadUserSecret(bytes.NewReader(append(line, '\n')), source)
}

func noPromptFlag(g GlobalOptions) string {
	if g.JSON {
		return "--json"
	}
	return "--non-interactive"
}

// readHiddenLine reads one line from the terminal stdin with echo off. If
// ctx ends first (Ctrl-C), it puts the terminal back as it was, since the
// read that turned echo off is still blocked, and returns ctx's cause.
func readHiddenLine(ctx context.Context, stdin io.Reader) ([]byte, error) {
	fd := stdin.(*os.File).Fd()
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := term.ReadPassword(fd)
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-ctx.Done():
		_ = term.Restore(fd, state)
		return nil, context.Cause(ctx)
	}
}
