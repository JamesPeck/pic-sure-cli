package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// stderrIsTerminal reports whether stderr, where questions go, is a
// terminal.
func (a *App) stderrIsTerminal() bool {
	if a.stderrTerminal != nil {
		return a.stderrTerminal()
	}
	return isTerminalWriter(a.Stderr)
}

// canConfirm reports whether a confirmation may be asked: no flag rules
// prompts out, and stdin, where the answer comes from, and stderr, where
// the question goes, are terminals. stdout may be redirected.
func (a *App) canConfirm() bool {
	g := a.Global
	return !g.Yes && !g.NonInteractive && !g.JSON && a.stdinIsTerminal() && a.stderrIsTerminal()
}

// ask writes prompt to stderr and reads one line of answer from stdin,
// without its line ending. A cancelled ctx (Ctrl-C) stops waiting and
// returns ctx's cause.
func (a *App) ask(ctx context.Context, prompt string) (string, error) {
	_, _ = io.WriteString(a.stderr(), prompt)
	type answer struct {
		line string
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		// On cancellation this read is left blocked until the process exits.
		line, err := bufio.NewReader(a.Stdin).ReadString('\n')
		got <- answer{line, err}
	}()
	select {
	case <-ctx.Done():
		_, _ = fmt.Fprintln(a.stderr())
		return "", context.Cause(ctx)
	case ans := <-got:
		if ans.err != nil && !errors.Is(ans.err, io.EOF) {
			return "", ans.err
		}
		if !strings.HasSuffix(ans.line, "\n") {
			_, _ = fmt.Fprintln(a.stderr())
		}
		return strings.TrimSpace(ans.line), nil
	}
}

// askYesNo asks question on stderr and reads the answer from stdin.
func (a *App) askYesNo(ctx context.Context, question string) (bool, error) {
	line, err := a.ask(ctx, question+" [y/N] ")
	if err != nil {
		return false, err
	}
	v := strings.ToLower(line)
	return v == "y" || v == "yes", nil
}
