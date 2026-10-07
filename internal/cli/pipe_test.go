package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/JamesPeck/pic-sure-cli/internal/events"
)

// closedPipe is a stream whose reader has gone: every write fails with
// EPIPE.
type closedPipe struct{ writes int }

func (p *closedPipe) Write([]byte) (int, error) {
	p.writes++
	return 0, syscall.EPIPE
}

// waitForCancel emits a step, then waits for the command's context to be
// cancelled, as a long operation would.
func waitForCancel(a *App, cancelled *bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		sink := a.newDeps().Sink
		sink.Emit(events.StepStarted{ID: "db", Title: "Start the database"})
		select {
		case <-cmd.Context().Done():
			*cancelled = true
			sink.Emit(events.StepDone{ID: "db", Status: events.StepFailed})
			return context.Cause(cmd.Context())
		case <-time.After(5 * time.Second):
			return errors.New("not cancelled")
		}
	}
}

func TestClosedPipeCancelsTheRun(t *testing.T) {
	t.Run("stdout under --json", func(t *testing.T) {
		a, _, stderr := testApp(t)
		pipe := &closedPipe{}
		a.Stdout = pipe
		var cancelled bool
		root := withRunE(t, a, []string{"up"}, waitForCancel(a, &cancelled))
		if code := a.execute(context.Background(), root, []string{"up", "--json"}); code != 141 {
			t.Errorf("exit = %d, want 141", code)
		}
		if !cancelled {
			t.Error("the command's context wasn't cancelled")
		}
		if pipe.writes != 1 {
			t.Errorf("%d writes to the closed stdout, want 1", pipe.writes)
		}
		if got := stderr.String(); !strings.Contains(got, "broken pipe") {
			t.Errorf("stderr = %q", got)
		}
	})
	t.Run("stderr in plain mode", func(t *testing.T) {
		a, stdout, _ := testApp(t)
		pipe := &closedPipe{}
		a.Stderr = pipe
		var cancelled bool
		root := withRunE(t, a, []string{"up"}, waitForCancel(a, &cancelled))
		if code := a.execute(context.Background(), root, []string{"up"}); code != 141 {
			t.Errorf("exit = %d, want 141", code)
		}
		if !cancelled || pipe.writes != 1 || stdout.Len() != 0 {
			t.Errorf("cancelled %v, %d writes, stdout %q", cancelled, pipe.writes, stdout)
		}
	})
	t.Run("a text report", func(t *testing.T) {
		a, _, _ := testApp(t)
		a.Stdout = &closedPipe{}
		root := withRunE(t, a, []string{"up"}, func(*cobra.Command, []string) error {
			return a.printReport(nil, func(w io.Writer) error {
				_, err := io.WriteString(w, "status\n")
				return err
			})
		})
		if code := a.execute(context.Background(), root, []string{"up"}); code != 141 {
			t.Errorf("exit = %d, want 141", code)
		}
	})
}
