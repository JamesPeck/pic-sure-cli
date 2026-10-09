package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

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

// readHiddenLine reads one line from the terminal stdin with echo off. It
// puts the terminal in raw mode before it starts reading, so that restoring
// it when ctx ends first can't race the read's own change; the read, still
// blocked, then changes nothing.
func readHiddenLine(ctx context.Context, stdin io.Reader) ([]byte, error) {
	f, ok := stdin.(*os.File)
	if !ok {
		return nil, errors.New("stdin isn't a terminal")
	}
	state, err := term.MakeRaw(f.Fd())
	if err != nil {
		return nil, err
	}
	defer func() { _ = term.Restore(f.Fd(), state) }()
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readRawLine(f)
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// readRawLine reads a line typed on a terminal in raw mode, which neither
// echoes nor edits: Enter ends it, Backspace and Ctrl-U edit it, Ctrl-C is
// an interrupt (raw mode sends it as a byte, not SIGINT), and Ctrl-D on an
// empty line is EOF.
func readRawLine(r io.Reader) ([]byte, error) {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 0 {
			if err == nil {
				continue
			}
			return nil, err
		}
		switch b[0] {
		case '\r', '\n':
			return line, nil
		case 3: // Ctrl-C
			return nil, exitcode.Signaled(os.Interrupt)
		case 4: // Ctrl-D
			if len(line) == 0 {
				return nil, io.EOF
			}
		case 0x15: // Ctrl-U
			line = line[:0]
		case 0x7f, '\b':
			if len(line) > 0 {
				_, size := utf8.DecodeLastRune(line)
				line = line[:len(line)-size]
			}
		default:
			line = append(line, b[0])
		}
	}
}
