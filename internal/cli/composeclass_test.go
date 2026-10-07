package cli

import (
	"regexp"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func TestComposeClass(t *testing.T) {
	ro, mut := stack.ReadOnly, stack.Mutating
	for _, tc := range []struct {
		args []string
		want stack.CommandClass
	}{
		{nil, mut},
		{[]string{"ps", "-a"}, ro},
		{[]string{"logs", "-f", "hpds"}, ro},
		{[]string{"exec", "hpds", "sh"}, ro},
		{[]string{"up", "-d"}, mut},
		{[]string{"cp", "hpds:/x", "."}, mut},
		{[]string{"nosuch"}, mut},
		{[]string{"help"}, mut},
		{[]string{"wait", "hpds"}, ro},
		{[]string{"wait", "--down-project", "hpds"}, mut},
		{[]string{"-p", "x", "wait", "hpds", "--down-project=true"}, mut},
		// A read-only word in a mutating subcommand's arguments.
		{[]string{"run", "ps"}, mut},
		// Global flags, with their values separate, joined or attached.
		{[]string{"-f", "up.yaml", "ps"}, ro},
		{[]string{"--file", "up", "logs"}, ro},
		{[]string{"-p", "up", "--profile", "up", "--env-file", "up", "ps"}, ro},
		{[]string{"--project-name=up", "--progress=plain", "--ansi", "never", "ps"}, ro},
		{[]string{"-fup.yaml", "-pdemo", "ps"}, ro},
		{[]string{"--parallel", "1", "--project-directory", "up", "--workdir", "up", "top"}, ro},
		{[]string{"--dry-run", "--compatibility", "--all-resources", "--no-ansi", "--verbose", "config"}, ro},
		{[]string{"--dry-run", "up"}, mut},
		{[]string{"-f", "x.yaml"}, mut},
		{[]string{"-f"}, mut},
		// An unknown flag may take a value, so what follows can't be trusted.
		{[]string{"--nosuch", "ps"}, mut},
		{[]string{"-x", "ps"}, mut},
		// Compose takes no subcommand after "--".
		{[]string{"--", "ps"}, mut},
		{[]string{"-f", "x.yaml", "--", "ps"}, mut},
	} {
		if got := composeClass(tc.args); got != tc.want {
			t.Errorf("composeClass(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// compose's help lists the subcommands that run without the lock.
func TestComposeHelpListsReadOnly(t *testing.T) {
	long := newComposeCmd(&App{}).Long
	for sub := range composeReadOnly {
		if !regexp.MustCompile(`\b` + sub + `\b`).MatchString(long) {
			t.Errorf("compose's help doesn't list %q", sub)
		}
	}
}
