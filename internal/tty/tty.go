// Package tty answers "may we prompt the user?".
package tty

import (
	"os"

	"github.com/mattn/go-isatty"
)

// IsInteractive reports whether both stdin and stdout are terminals.
// Prompting is only allowed when this is true; otherwise commands must fail
// fast naming the flags that replace the prompt.
func IsInteractive() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

// isTerminal reports whether f is a terminal. A character-device check is
// not enough: /dev/null is a character device, and cron, systemd and CI
// often attach it to stdin.
func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd())
}
