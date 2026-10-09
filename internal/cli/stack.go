package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

// openStack opens the stack cmd acts on: --stack DIR, or the one containing
// the current directory. No stack there is exit 3. It then applies the
// version gate for cmd's class (gate.go), so a command the gate refuses
// gets exit 5, and starts the run's log file in the stack (openRunLog).
func (a *App) openStack(cmd *cobra.Command) (*stack.Stack, error) {
	st, err := a.openStackUnlogged(cmd)
	if err != nil {
		return nil, err
	}
	a.openRunLog(st)
	return st, nil
}

// openStackUnlogged is openStack without the run log, for a command that
// must change nothing in the stack until the user confirms.
func (a *App) openStackUnlogged(cmd *cobra.Command) (*stack.Stack, error) {
	if a.noStack {
		return nil, stack.ErrNotFound
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir, err := stack.Find(a.Global.Stack, cwd)
	if err != nil {
		return nil, err
	}
	st, err := stack.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := a.gate(cmd, st); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// initDir returns the directory init creates its stack in, from its
// arguments and --stack (D14).
func (a *App) initDir(args []string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	return stack.InitDir(arg, a.Global.Stack, cwd)
}

// lockStack takes st's lock for a mutating command (§10.5). If another
// command holds it, that is exit 1, or with --wait-lock a wait, announced on
// sink. Holding the lock, it applies the version gate again: the holder it
// waited for may have been a newer pic-sure that re-rendered the stack.
// Commands that change the stack's Docker resources then make the
// ownership check (checkOwned, or ops.CheckOwnership inside the operation).
func (a *App) lockStack(ctx context.Context, cmd *cobra.Command, st *stack.Stack, sink events.Sink) (*stack.Lock, error) {
	l, err := st.Lock(ctx, stack.LockOptions{
		Wait:    a.Global.WaitLock,
		Command: cmd.CommandPath(),
		OnWait: func(holder string) {
			sink.Emit(events.Warning{Text: "waiting for " + holder + " to finish with the stack"})
		},
	})
	if errors.Is(err, stack.ErrLocked) {
		return nil, exitcode.Failed("%w; try again when it finishes, or pass --wait-lock to wait for it", err)
	}
	if err != nil {
		return nil, err
	}
	if err := a.gate(cmd, st); err != nil {
		_ = l.Unlock()
		return nil, err
	}
	// A stack from before stack IDs gets one now, so the resources this
	// command makes carry it (§6.1).
	if _, err := st.EnsureID(rand.Reader); err != nil {
		_ = l.Unlock()
		return nil, err
	}
	return l, nil
}

// checkOwned is the ownership check (§6.1) of a command that changes the
// stack's Docker resources: holding the lock, after the command's own
// preconditions and before its first change, the stack's name selecting
// another stack's resources is exit 3.
func checkOwned(cmd *cobra.Command, d *ops.Deps, st *stack.Stack, cfg *stack.Config) error {
	_, err := ops.CheckOwnership(cmd.Context(), d, st, cfg.Name)
	return err
}

// checkOwnedStack is checkOwned for a command that hasn't loaded the
// config.
func checkOwnedStack(cmd *cobra.Command, d *ops.Deps, st *stack.Stack) error {
	cfg, err := st.LoadConfig()
	if err != nil {
		return configError(err)
	}
	return checkOwned(cmd, d, st, cfg)
}

// warnForeign warns, for a read-only command, when the stack's name
// selects another stack's Docker resources (§6.1), whose containers
// compose would then show. It says nothing when it can't tell.
func (a *App) warnForeign(ctx context.Context, d *ops.Deps, st *stack.Stack) {
	cfg, err := st.LoadConfig()
	if err != nil {
		return
	}
	o, err := ops.StackResources(ctx, d, cfg.Name, st.ID(), st.Dir)
	if err != nil || len(o.Foreign()) == 0 {
		return
	}
	a.warnStderr("the stack name %s is in use by another stack's Docker resources, so what follows may be theirs:\n%s", cfg.Name, ops.ResourceList(ops.Refs(o.Foreign())))
}
