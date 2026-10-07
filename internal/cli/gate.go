package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// commandClasses is every command's class for the version gate (§10.6),
// keyed by its path without "pic-sure". The read-only ones are §10.6's
// list, including the list subcommands; everything else is mutating, and
// update migrates. compose's entry stands for its mutating subcommands.
// openStack applies the gate, so a command that never opens a stack, such
// as version or cache, isn't gated whatever its class.
var commandClasses = map[string]stack.CommandClass{
	"status":           stack.ReadOnly,
	"ps":               stack.ReadOnly,
	"logs":             stack.ReadOnly,
	"doctor":           stack.ReadOnly,
	"support-bundle":   stack.ReadOnly,
	"config show":      stack.ReadOnly,
	"config get":       stack.ReadOnly,
	"version":          stack.ReadOnly,
	"dev list":         stack.ReadOnly,
	"shared-data list": stack.ReadOnly,
	"cache list":       stack.ReadOnly,

	"update": stack.Migrating,

	"init":                   stack.Mutating,
	"up":                     stack.Mutating,
	"down":                   stack.Mutating,
	"restart":                stack.Mutating,
	"compose":                stack.Mutating,
	"build":                  stack.Mutating,
	"migrate":                stack.Mutating,
	"config set":             stack.Mutating,
	"config edit":            stack.Mutating,
	"secrets rotate":         stack.Mutating,
	"data demo":              stack.Mutating,
	"data load-phenotype":    stack.Mutating,
	"data load-genomic":      stack.Mutating,
	"dictionary hydrate":     stack.Mutating,
	"dictionary load-csv":    stack.Mutating,
	"dictionary load-facets": stack.Mutating,
	"dictionary weights":     stack.Mutating,
	"shared-data publish":    stack.Mutating,
	"shared-data remove":     stack.Mutating,
	"dev on":                 stack.Mutating,
	"dev off":                stack.Mutating,
	"db bootstrap":           stack.Mutating,
	"reset":                  stack.Mutating,
	"destroy":                stack.Mutating,
	"cache prune":            stack.Mutating,
	"self-update":            stack.Mutating,
}

// commandClass returns cmd's class. compose's depends on its compose
// subcommand (composeClass). A command missing from the table is mutating,
// the safe side; a test keeps the table complete.
func commandClass(cmd *cobra.Command) stack.CommandClass {
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	if path == "compose" {
		return composeClass(cmd.Flags().Args())
	}
	if c, ok := commandClasses[path]; ok {
		return c
	}
	return stack.Mutating
}

func (a *App) configMigrations() stack.Registry {
	if a.migrations != nil {
		return *a.migrations
	}
	return stack.ConfigMigrations()
}

// gate applies the version gate (§10.6) for cmd to st, printing a read-only
// command's warning to stderr. A read-only command that can't read
// state.json runs anyway, with a warning, since it may be what reports the
// problem.
func (a *App) gate(cmd *cobra.Command, st *stack.Stack) error {
	class := commandClass(cmd)
	v, err := st.CheckVersions(a.Info.Version, a.configMigrations())
	if err != nil {
		if class == stack.ReadOnly {
			a.warnStderr("can't check which pic-sure this stack was rendered by: %v", err)
			return nil
		}
		return exitcode.Failed("%w", err)
	}
	warning, err := v.Gate(class)
	if warning != "" {
		a.warnStderr("%s", warning)
	}
	return err
}

// warnStderr goes to stderr because stdout may carry --json output.
func (a *App) warnStderr(format string, args ...any) {
	_, _ = fmt.Fprintf(a.stderr(), "pic-sure: warning: "+format+"\n", args...)
}
