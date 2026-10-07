package cli

import (
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// composeReadOnly is the compose subcommands that change nothing in the
// stack, so `compose -- ARGS` runs them without the stack lock and as a
// read-only command for the version gate (§10.6).
var composeReadOnly = map[string]bool{
	"ps": true, "logs": true, "top": true, "config": true, "events": true,
	"images": true, "ls": true, "port": true, "version": true, "exec": true,
	"stats": true, "wait": true, "attach": true,
}

// composeValueFlags and composeBoolFlags are compose's global flags, from
// `docker compose --help` (2.29 to 5.x, plus the hidden --workdir,
// --no-ansi and --verbose).
var (
	composeValueFlags = map[string]bool{
		"-f": true, "--file": true, "-p": true, "--project-name": true,
		"--profile": true, "--env-file": true, "--progress": true, "--ansi": true,
		"--parallel": true, "--project-directory": true, "--workdir": true,
	}
	composeBoolFlags = map[string]bool{
		"--all-resources": true, "--compatibility": true, "--dry-run": true,
		"--no-ansi": true, "--verbose": true,
	}
)

// composeClass classifies `compose -- args` by its compose subcommand, the
// first argument after compose's global flags. Anything it can't place (no
// subcommand, an unknown subcommand or global flag, a "--") is mutating.
func composeClass(args []string) stack.CommandClass {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if composeReadOnly[arg] {
				return stack.ReadOnly
			}
			return stack.Mutating
		}
		name, _, hasValue := strings.Cut(arg, "=")
		switch {
		case composeValueFlags[name]:
			if !hasValue {
				i++
			}
		case len(name) > 2 && name[1] != '-' && composeValueFlags[name[:2]]:
			// -fFILE, -pNAME
		case composeBoolFlags[name]:
		default:
			return stack.Mutating
		}
	}
	return stack.Mutating
}
