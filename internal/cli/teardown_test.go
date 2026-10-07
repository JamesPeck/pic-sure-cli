package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

func TestConfirmName(t *testing.T) {
	cmd := &cobra.Command{Use: "destroy"}
	for _, tc := range []struct {
		name     string
		terminal bool
		yes      bool
		input    string
		want     int
	}{
		{"typed the name", true, false, "demo\n", exitcode.CodeOK},
		{"typed without a newline", true, false, "demo", exitcode.CodeOK},
		{"typed something else", true, false, "dem\n", exitcode.CodeConfirmRequired},
		{"typed nothing", true, false, "", exitcode.CodeConfirmRequired},
		{"no terminal", false, false, "demo\n", exitcode.CodeConfirmRequired},
		{"--yes", false, true, "", exitcode.CodeOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, stderr := testApp(t)
			a.IsTerminal = func() bool { return tc.terminal }
			a.Global.Yes = tc.yes
			a.Stdin = strings.NewReader(tc.input)
			err := a.confirmName(cmd, "demo", "This removes stack demo.")
			if got := exitcode.FromError(err); got != tc.want {
				t.Errorf("exit = %d (%v), want %d", got, err, tc.want)
			}
			asked := strings.Contains(stderr.String(), "Type the stack name (demo) to confirm: ")
			if asked != (tc.terminal && !tc.yes) {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}
