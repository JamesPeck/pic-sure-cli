//go:build smoketest

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
	"github.com/JamesPeck/pic-sure-cli/internal/steps"
)

func init() { extraCommands = append(extraCommands, newSmokeStepsCmd) }

// newSmokeStepsCmd is a command that emits step events without Docker, for
// the PTY smoke tests of the TUI renderer.
func newSmokeStepsCmd(a *App) *cobra.Command {
	var fail, wait, hang bool
	cmd := &cobra.Command{
		Use:    "smoke-steps",
		Short:  "Run steps that emit events, for smoke tests",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := a.newDeps()
			pause := func(ctx context.Context) error {
				select {
				case <-ctx.Done():
					return context.Cause(ctx)
				case <-time.After(20 * time.Millisecond):
					return nil
				}
			}
			plan := []steps.Step{{
				ID:    "prepare",
				Title: "Prepare the stack",
				Check: func(context.Context) (bool, error) { return true, nil },
				Apply: func(context.Context, events.Sink) error { return nil },
			}, {
				ID:    "fetch",
				Title: "Fetch sources",
				Apply: func(ctx context.Context, sink events.Sink) error {
					for i := 0; i <= 100; i += 25 {
						pct := float64(i)
						sink.Emit(events.Progress{ID: "fetch", Text: "pic-sure", Pct: &pct})
						sink.Emit(events.Log{ID: "fetch", Stream: "stdout", Line: fmt.Sprintf("fetch line %d", i)})
						if err := pause(ctx); err != nil {
							return err
						}
					}
					sink.Emit(events.Warning{ID: "fetch", Text: "a warning from fetch"})
					d.Log.Warn("a log record during fetch")
					return nil
				},
			}, {
				ID:    "build",
				Title: "Build images",
				Apply: func(ctx context.Context, sink events.Sink) error {
					for i := 1; i <= 30; i++ {
						sink.Emit(events.Log{ID: "build", Stream: "stdout", Line: fmt.Sprintf("build line %d", i)})
						if err := pause(ctx); err != nil {
							return err
						}
					}
					if hang {
						sink.Emit(events.Progress{ID: "build", Text: "ignoring cancellation"})
						select {}
					}
					if wait {
						sink.Emit(events.Progress{ID: "build", Text: "waiting for cancellation"})
						<-ctx.Done()
						sink.Emit(events.Log{ID: "build", Stream: "stderr", Line: "cleaning up after cancellation"})
						return context.Cause(ctx)
					}
					if fail {
						return errors.New("the build failed")
					}
					return nil
				},
			}, {
				ID:    "finish",
				Title: "Finish",
				Apply: func(context.Context, events.Sink) error { return nil },
			}}
			if err := steps.Run(cmd.Context(), d.Sink, plan, steps.Options{Skip: a.Global.SkipSteps}); err != nil {
				return err
			}
			return a.finish(nil, func(w io.Writer) error {
				_, err := fmt.Fprintln(w, "smoke-steps finished")
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&fail, "fail", false, "fail the build step")
	cmd.Flags().BoolVar(&wait, "wait", false, "block in the build step until cancelled")
	cmd.Flags().BoolVar(&hang, "hang", false, "block in the build step for good")
	return cmd
}
