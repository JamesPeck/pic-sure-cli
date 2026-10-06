// Command pic-sure installs, runs, updates, loads data into, and tears down
// PIC-SURE All-in-One stacks. This is v2, the native Go implementation; see
// docs/architecture.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/JamesPeck/pic-sure-cli/internal/cli"
	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

// Injected via -ldflags (see Makefile).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run executes the CLI and returns its exit code. The first SIGINT or
// SIGTERM cancels the command's context, so deferred cleanups run and the
// command exits 128+N; a second signal gets the default action and kills
// the process at once (spec §10.5).
//
// This is signal.NotifyContext done by hand: NotifyContext's cancellation
// cause doesn't say which signal arrived, and the exit code needs it.
func run(args []string) int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case sig := <-sigs:
			signal.Stop(sigs)
			cancel(exitcode.Signaled(sig))
		case <-ctx.Done():
		}
	}()

	return cli.Execute(ctx, cli.BuildInfo{Version: version, Commit: commit, Date: date}, args)
}
