package tty

import (
	"os"
	"testing"

	"github.com/creack/pty"
)

func TestIsTerminal(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	if isTerminal(devNull) {
		t.Error("/dev/null counted as a terminal")
	}

	ptmx, term, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	defer func() { _ = ptmx.Close(); _ = term.Close() }()
	if !isTerminal(term) {
		t.Error("a pseudo-terminal was not counted as a terminal")
	}
}
