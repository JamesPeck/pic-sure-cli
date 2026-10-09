package cli

import (
	"strings"

	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// composeReadOnly is the compose subcommands that change nothing in the
// stack, so `compose -- ARGS` runs them without the stack lock and as a
// read-only command for the version gate (§10.6). wait --down-project is
// the exception: it takes the project down.
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

// composeClass classifies `compose -- args` by its compose subcommand
// (composeSubcommand). Anything it can't place (no subcommand, an unknown
// subcommand or global flag, a "--") is mutating.
func composeClass(args []string) stack.CommandClass {
	i := composeSubcommand(args)
	if i < 0 || !composeReadOnly[args[i]] || args[i] == "wait" && downsProject(args[i+1:]) {
		return stack.Mutating
	}
	return stack.ReadOnly
}

// composeSubcommand returns the index in args of the compose subcommand,
// the first argument after compose's global flags, or -1 when there is
// none or a global flag it doesn't know (or a "--") comes first.
func composeSubcommand(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return i
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
			return -1
		}
	}
	return -1
}

func downsProject(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if strings.HasPrefix(arg, "--down-project") {
			return true
		}
	}
	return false
}

// composeProjectFlag returns the -p or --project-name among compose's
// global flags in args, or "". pic-sure sets the project from the stack's
// name, and the ownership check (§6.1) covers only that project.
func composeProjectFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return ""
		}
		name, _, hasValue := strings.Cut(arg, "=")
		switch {
		case name == "-p" || name == "--project-name" || strings.HasPrefix(name, "-p") && len(name) > 2 && name[1] != '-':
			return arg
		case composeValueFlags[name] && !hasValue:
			i++
		}
	}
	return ""
}
